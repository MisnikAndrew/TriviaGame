package main

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestLimiterBurstThenRefill(t *testing.T) {
	now := time.Unix(1000, 0)
	l := newLimiter(2, 3)
	l.now = func() time.Time { return now }

	for i := range 3 {
		if !l.allow("a") {
			t.Fatalf("запрос %d в пределах всплеска отклонён", i+1)
		}
	}
	if l.allow("a") {
		t.Error("четвёртый запрос подряд должен упереться в лимит")
	}
	if !l.allow("b") {
		t.Error("лимит одного адреса не должен задевать другой")
	}
	now = now.Add(500 * time.Millisecond) // 2/с → один токен
	if !l.allow("a") || l.allow("a") {
		t.Error("за полсекунды должен вернуться ровно один токен")
	}
	now = now.Add(time.Hour)
	l.allow("c") // запускает уборку
	if len(l.buckets) != 1 {
		t.Errorf("давно не звучавшие адреса надо забывать, осталось %d", len(l.buckets))
	}

	var off *limiter
	if !off.allow("x") {
		t.Error("nil-лимитер ничего не ограничивает")
	}
}

func TestStreamCounter(t *testing.T) {
	c := newStreamCounter(2)
	if !c.acquire("a") || !c.acquire("a") || c.acquire("a") {
		t.Fatal("с одного адреса должно пускать ровно 2 соединения")
	}
	if !c.acquire("b") {
		t.Error("соединения разных адресов считаются отдельно")
	}
	c.release("a")
	if !c.acquire("a") {
		t.Error("после закрытия соединения место освобождается")
	}
	var off *streamCounter
	if !off.acquire("x") {
		t.Error("nil-счётчик ничего не ограничивает")
	}
	off.release("x")
}

func TestClientIP(t *testing.T) {
	req := func(remote string, hdr map[string]string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name  string
		r     *http.Request
		trust bool
		want  string
	}{
		{"напрямую", req("203.0.113.7:5555", nil), false, "203.0.113.7"},
		{"IPv6", req("[2001:db8::1]:5555", nil), false, "2001:db8::1"},
		{"заголовки без доверия игнорируются", req("127.0.0.1:1", map[string]string{"X-Forwarded-For": "1.2.3.4", "CF-Connecting-IP": "5.6.7.8"}), false, "127.0.0.1"},
		{"Cloudflare", req("127.0.0.1:1", map[string]string{"CF-Connecting-IP": "198.51.100.9"}), true, "198.51.100.9"},
		{"последний в X-Forwarded-For", req("127.0.0.1:1", map[string]string{"X-Forwarded-For": "6.6.6.6, 198.51.100.4"}), true, "198.51.100.4"},
		{"мусор в заголовке", req("127.0.0.1:1", map[string]string{"X-Forwarded-For": "не адрес"}), true, "127.0.0.1"},
		{"доверие без заголовков", req("203.0.113.7:5555", nil), true, "203.0.113.7"},
	}
	for _, tc := range cases {
		if got := clientIP(tc.r, tc.trust); got != tc.want {
			t.Errorf("%s: %q, ожидали %q", tc.name, got, tc.want)
		}
	}
}

func TestLimitKeyGroupsIPv6By64(t *testing.T) {
	if limitKey("2001:db8:1:2::1") != limitKey("2001:db8:1:2:ffff::9") {
		t.Error("адреса одной сети /64 должны считаться одним клиентом")
	}
	if limitKey("2001:db8:1:2::1") == limitKey("2001:db8:1:3::1") {
		t.Error("разные сети /64 — разные клиенты")
	}
	if limitKey("203.0.113.7") != "203.0.113.7" {
		t.Error("IPv4 остаётся как есть")
	}
}

func TestNormalizePublicURL(t *testing.T) {
	good := map[string]string{
		"":                              "",
		"https://quiz.example.com":      "https://quiz.example.com",
		"https://quiz.example.com/":     "https://quiz.example.com",
		"http://203.0.113.7:8080":       "http://203.0.113.7:8080",
		"https://a-b.trycloudflare.com": "https://a-b.trycloudflare.com",
	}
	for in, want := range good {
		if got, err := normalizePublicURL(in); err != nil || got != want {
			t.Errorf("%q → %q, %v; ожидали %q", in, got, err, want)
		}
	}
	for _, in := range []string{"quiz.example.com", "ftp://quiz.example.com", "https://", "https://quiz.example.com/game", "https://quiz.example.com/?a=1", "https://user:pw@quiz.example.com"} {
		if got, err := normalizePublicURL(in); err == nil {
			t.Errorf("%q должен быть отклонён, получили %q", in, got)
		}
	}
}

// internetServer is a Server set up like main does for a public host, with
// the knobs a test wants to turn.
func internetServer(t *testing.T, tune func(*Server)) (*httptest.Server, *Server) {
	t.Helper()
	h := testHub(t)
	web, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{hub: h, cfg: h.cfg, bank: h.bank, urls: []string{"http://192.168.1.5:8080"}, web: web}
	tune(s)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func getInfo(t *testing.T, ts *httptest.Server, hdr map[string]string) (urls []any, publicURL string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/info", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct {
		URLs      []any  `json:"urls"`
		PublicURL string `json:"publicUrl"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	return info.URLs, info.PublicURL
}

func TestInfoHidesLocalAddressesFromTheInternet(t *testing.T) {
	ts, _ := internetServer(t, func(s *Server) {
		s.publicURL = "https://quiz.example.com"
		s.trustProxy = true
	})
	if urls, pub := getInfo(t, ts, nil); len(urls) != 1 || pub != "https://quiz.example.com" {
		t.Errorf("с этого же компьютера: urls=%v publicUrl=%q", urls, pub)
	}
	if urls, pub := getInfo(t, ts, map[string]string{"CF-Connecting-IP": "198.51.100.9"}); len(urls) != 0 || pub != "https://quiz.example.com" {
		t.Errorf("из интернета адреса домашней сети видны: urls=%v publicUrl=%q", urls, pub)
	}
	if urls, _ := getInfo(t, ts, map[string]string{"CF-Connecting-IP": "192.168.1.20"}); len(urls) != 1 {
		t.Errorf("из домашней сети адреса нужны: urls=%v", urls)
	}
}

func TestRateLimitedActions(t *testing.T) {
	ts, _ := internetServer(t, func(s *Server) {
		s.actionLim = newLimiter(0.001, 3)
		s.joinLim = newLimiter(0.001, 2)
	})
	post := func(path string) int {
		resp, err := http.Post(ts.URL+path, "application/json", strings.NewReader(`{"pid":"unknown-player-1","code":"ABCD"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := range 3 {
		if code := post("/api/name"); code != http.StatusNotFound { // игрока нет, но до лимита дошли
			t.Fatalf("запрос %d: статус %d", i+1, code)
		}
	}
	if code := post("/api/name"); code != http.StatusTooManyRequests {
		t.Errorf("четвёртый запрос подряд: статус %d, ожидали 429", code)
	}

	ts2, _ := internetServer(t, func(s *Server) { s.joinLim = newLimiter(0.001, 2) })
	join := func() int {
		resp, err := http.Post(ts2.URL+"/api/join", "application/json", strings.NewReader(`{"pid":"unknown-player-1","code":"ABCD"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	join()
	join()
	if code := join(); code != http.StatusTooManyRequests {
		t.Errorf("перебор кодов комнат не ограничен: статус %d", code)
	}
}

func TestStreamAndPlayerCaps(t *testing.T) {
	ts, s := internetServer(t, func(s *Server) { s.streams = newStreamCounter(1) })
	s.hub.maxClients = 2

	first := openSSE(t, ts.URL, "player-one-id", "A")
	first.waitFor(t, func(ClientState) bool { return true })

	resp, err := http.Get(ts.URL + "/api/events?pid=player-two-id&name=B")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("второе соединение с того же адреса: статус %d, ожидали 429", resp.StatusCode)
	}

	// лимит игроков: третий новый игрок не проходит, знакомый — проходит
	s.streams = nil
	second := openSSE(t, ts.URL, "player-two-id", "B")
	second.waitFor(t, func(ClientState) bool { return true })
	resp, err = http.Get(ts.URL + "/api/events?pid=player-three-id&name=C")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("сервер переполнен: статус %d, ожидали 503", resp.StatusCode)
	}
	if !s.hub.canAccept("player-one-id") {
		t.Error("уже известного игрока переполнение не касается")
	}
}

// the second guarded handler must not break normal play when no limits are set
func TestZeroServerIsUnlimited(t *testing.T) {
	ts, _ := internetServer(t, func(*Server) {})
	for range 100 {
		resp, err := http.Get(ts.URL + "/api/info")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("статус %d без включённых лимитов", resp.StatusCode)
		}
	}
}
