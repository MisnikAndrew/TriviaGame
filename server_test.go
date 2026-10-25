package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseClient is a minimal EventSource: it parses the stream into ClientStates.
type sseClient struct {
	states chan ClientState
	events chan string // names of non-state events ("replaced")
	resp   *http.Response
}

func openSSE(t *testing.T, base, pid, name string) *sseClient {
	t.Helper()
	resp, err := http.Get(base + "/api/events?pid=" + pid + "&name=" + name)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	c := &sseClient{states: make(chan ClientState, 64), events: make(chan string, 4), resp: resp}
	go func() {
		defer close(c.states)
		sc := bufio.NewScanner(resp.Body)
		event := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if event == "state" {
					var st ClientState
					if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &st) == nil {
						c.states <- st
					}
				} else if event != "" {
					c.events <- event
				}
			}
		}
	}()
	t.Cleanup(func() { resp.Body.Close() })
	return c
}

func (c *sseClient) waitFor(t *testing.T, cond func(ClientState) bool) ClientState {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case st, ok := <-c.states:
			if !ok {
				t.Fatal("поток событий закрылся")
			}
			if cond(st) {
				return st
			}
		case <-timeout:
			t.Fatal("ожидаемое состояние не пришло")
		}
	}
}

func post(t *testing.T, base, path string, body map[string]any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(base+"/api/"+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func testServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	h := testHub(t)
	web, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{hub: h, cfg: h.cfg, bank: h.bank, urls: []string{"http://192.168.1.5:8080"}, web: web}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func TestHTTPServesFrontendAndInfo(t *testing.T) {
	ts, _ := testServer(t)

	for path, want := range map[string]string{"/": "<title>Борьба умов</title>", "/app.js": "EventSource", "/style.css": ".opt"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), want) {
			t.Errorf("GET %s: статус %d, нет %q", path, resp.StatusCode, want)
		}
	}

	resp, err := http.Get(ts.URL + "/api/info")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var info struct {
		URLs       []string `json:"urls"`
		Rounds     int      `json:"rounds"`
		Categories []struct {
			Name  string `json:"name"`
			Count int    `json:"count"`
		} `json:"categories"`
		Questions int `json:"questions"`
		Points    int `json:"points"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		t.Fatal(err)
	}
	if info.Rounds != 3 || len(info.Categories) < 8 || info.Questions < 100 || len(info.URLs) != 1 || info.Points != correctPoints {
		t.Errorf("странный /api/info: %+v", info)
	}
}

func TestHTTPRejectsBadRequests(t *testing.T) {
	ts, _ := testServer(t)

	if resp, _ := http.Get(ts.URL + "/api/events?pid=x"); resp.StatusCode != 400 {
		t.Errorf("короткий pid: статус %d, ожидался 400", resp.StatusCode)
	}
	if code, _ := post(t, ts.URL, "quick", map[string]any{"pid": "player-unknown"}); code != 404 {
		t.Errorf("неизвестный игрок: статус %d, ожидался 404", code)
	}
	resp, _ := http.Post(ts.URL+"/api/quick", "application/json", strings.NewReader("{oops"))
	if resp.StatusCode != 400 {
		t.Errorf("битый JSON: статус %d, ожидался 400", resp.StatusCode)
	}
	if resp, _ := http.Get(ts.URL + "/api/quick"); resp.StatusCode == 200 {
		t.Error("GET на POST-ручку не должен срабатывать")
	}
}

// TestHTTPFullGameVsBot plays a whole match through the real endpoints.
func TestHTTPFullGameVsBot(t *testing.T) {
	ts, s := testServer(t)
	const pid = "player-http-1"
	c := openSSE(t, ts.URL, pid, "Вика")

	st := c.waitFor(t, func(st ClientState) bool { return true })
	if st.Screen != "menu" || st.Name != "Вика" {
		t.Fatalf("ожидалось меню для Вики, получили %+v", st)
	}
	if code, body := post(t, ts.URL, "name", map[string]any{"pid": pid, "name": "  Виктория  "}); code != 200 {
		t.Fatalf("name: %d %s", code, body)
	}
	if st := c.waitFor(t, func(st ClientState) bool { return st.Name == "Виктория" }); st.Screen != "menu" {
		t.Fatalf("переименование: %+v", st)
	}
	if code, body := post(t, ts.URL, "bot", map[string]any{"pid": pid}); code != 200 {
		t.Fatalf("bot: %d %s", code, body)
	}

	revealed := 0
	for {
		st := c.waitFor(t, func(st ClientState) bool { return st.Game != nil })
		g := st.Game
		if st.Screen != "game" {
			t.Fatalf("screen = %q", st.Screen)
		}
		switch g.Phase {
		case "pick":
			if g.IPick {
				post(t, ts.URL, "pick", map[string]any{"pid": pid, "index": 1})
			}
		case "question":
			if g.me().Answer < 0 {
				post(t, ts.URL, "answer", map[string]any{"pid": pid, "seq": g.Question.Seq, "index": 0})
			}
		case "reveal":
			revealed++
		}
		if g.Phase == "final" {
			break
		}
	}
	if want := s.cfg.Rounds * s.cfg.QPerRound; revealed != want {
		t.Errorf("раскрыто вопросов: %d, ожидалось %d", revealed, want)
	}
	if code, _ := post(t, ts.URL, "leave", map[string]any{"pid": pid}); code != 200 {
		t.Error("leave")
	}
	c.waitFor(t, func(st ClientState) bool { return st.Screen == "menu" })
}

func TestHTTPPauseEndpoint(t *testing.T) {
	ts, _ := testServer(t)
	const pid = "player-http-3"
	c := openSSE(t, ts.URL, pid, "Вика")
	c.waitFor(t, func(ClientState) bool { return true })

	if code, _ := post(t, ts.URL, "pause", map[string]any{"pid": pid, "on": true}); code != 409 {
		t.Errorf("пауза вне игры: статус %d, ожидался 409", code)
	}
	post(t, ts.URL, "bot", map[string]any{"pid": pid})
	c.waitFor(t, func(st ClientState) bool { return st.Game != nil && st.Game.Phase == "pick" })

	if code, _ := post(t, ts.URL, "pause", map[string]any{"pid": pid, "on": true}); code != 200 {
		t.Fatalf("пауза: статус %d", code)
	}
	st := c.waitFor(t, func(st ClientState) bool { return st.Game != nil && st.Game.Paused })
	if !st.Game.IPaused || st.Game.PausedBy != "Вика" {
		t.Errorf("пауза должна быть от Вики: %+v", st.Game)
	}
	post(t, ts.URL, "pause", map[string]any{"pid": pid, "on": false})
	c.waitFor(t, func(st ClientState) bool { return st.Game != nil && st.Game.Phase == "pick" && !st.Game.Paused })
	post(t, ts.URL, "leave", map[string]any{"pid": pid})
}

func TestHTTPCreateRoomWithRules(t *testing.T) {
	ts, _ := testServer(t)
	const pid = "player-http-4"
	c := openSSE(t, ts.URL, pid, "Лена")
	c.waitFor(t, func(ClientState) bool { return true })

	if code, _ := post(t, ts.URL, "room", map[string]any{"pid": pid, "seconds": 2}); code != 409 {
		t.Errorf("слишком короткое время: статус %d, ожидался 409", code)
	}
	if code, _ := post(t, ts.URL, "room", map[string]any{"pid": pid, "seconds": 45, "speed": true}); code != 200 {
		t.Fatalf("создание комнаты: статус %d", code)
	}
	st := c.waitFor(t, func(st ClientState) bool { return st.Screen == "room" })
	if st.Rules == nil || *st.Rules != (RulesView{Seconds: 45, Speed: true, Rounds: 3, QPerRound: 2}) {
		t.Errorf("правила комнаты: %+v", st.Rules)
	}
	post(t, ts.URL, "cancel", map[string]any{"pid": pid})
	c.waitFor(t, func(st ClientState) bool { return st.Screen == "menu" })

	// запрос без правил — комната по умолчанию: время сервера, скорость не считается
	post(t, ts.URL, "room", map[string]any{"pid": pid})
	if st := c.waitFor(t, func(st ClientState) bool { return st.Screen == "room" }); st.Rules == nil || st.Rules.Speed {
		t.Errorf("правила по умолчанию: %+v", st.Rules)
	}
}

func TestHTTPLobbyStartAndHint(t *testing.T) {
	ts, _ := testServer(t)
	const a, b = "player-http-5", "player-http-6"
	ca := openSSE(t, ts.URL, a, "Аня")
	cb := openSSE(t, ts.URL, b, "Боря")
	ca.waitFor(t, func(ClientState) bool { return true })
	cb.waitFor(t, func(ClientState) bool { return true })

	post(t, ts.URL, "room", map[string]any{"pid": a, "bots": 1, "hints": true, "seconds": 5})
	code := ca.waitFor(t, func(st ClientState) bool { return st.Screen == "room" }).Room
	if c, _ := post(t, ts.URL, "join", map[string]any{"pid": b, "code": code}); c != 200 {
		t.Fatalf("вход в комнату: статус %d", c)
	}
	st := ca.waitFor(t, func(st ClientState) bool { return st.Lobby != nil && len(st.Lobby.Players) == 3 })
	if !st.Lobby.Host || !st.Lobby.CanStart || !st.Rules.Hints {
		t.Errorf("лобби создателя: %+v %+v", st.Lobby, st.Rules)
	}
	if c, _ := post(t, ts.URL, "start", map[string]any{"pid": b}); c != 409 {
		t.Errorf("гость запускает игру: статус %d, ожидался 409", c)
	}
	if c, _ := post(t, ts.URL, "hint", map[string]any{"pid": a, "kind": "fifty"}); c != 409 {
		t.Errorf("подсказка вне игры: статус %d, ожидался 409", c)
	}
	if c, _ := post(t, ts.URL, "start", map[string]any{"pid": a}); c != 200 {
		t.Fatalf("старт: статус %d", c)
	}
	for _, c := range []*sseClient{ca, cb} {
		g := c.waitFor(t, func(st ClientState) bool { return st.Game != nil && st.Game.Phase == "pick" }).Game
		if len(g.Players) != 3 || !g.Rules.Hints {
			t.Errorf("игра на троих с подсказками: %+v", g)
		}
	}
	if c, _ := post(t, ts.URL, "hint", map[string]any{"pid": a, "kind": "bogus"}); c != 409 {
		t.Errorf("неизвестная подсказка: статус %d, ожидался 409", c)
	}
	post(t, ts.URL, "leave", map[string]any{"pid": a})
	post(t, ts.URL, "leave", map[string]any{"pid": b})
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHTTPProfilesAreOffWithoutADatabase(t *testing.T) {
	ts, _ := testServer(t)
	c := openSSE(t, ts.URL, "player-http-7", "Тест")
	c.waitFor(t, func(ClientState) bool { return true })
	if code, _ := post(t, ts.URL, "login", map[string]any{"pid": "player-http-7"}); code != 404 {
		t.Errorf("вход без базы: статус %d, ожидался 404", code)
	}
	if code, _ := get(t, ts.URL+"/api/top"); code != 404 {
		t.Errorf("топ без базы: статус %d", code)
	}
	if code, body := get(t, ts.URL+"/api/info"); code != 200 || !strings.Contains(body, `"profiles":false`) {
		t.Errorf("info без базы: %d %s", code, body)
	}
}

func TestHTTPLoginProfileAndTop(t *testing.T) {
	ts, srv := testServer(t)
	srv.hub.store = testStore(t)
	const pid = "player-http-8"
	c := openSSE(t, ts.URL, pid, "Лена")
	c.waitFor(t, func(ClientState) bool { return true })

	type loginReply struct{ UID, Secret, Name string }
	login := func(body map[string]any) (int, loginReply) {
		code, out := post(t, ts.URL, "login", body)
		var r loginReply
		json.Unmarshal([]byte(out), &r)
		return code, r
	}

	if code, _ := login(map[string]any{"pid": "player-unknown"}); code != 404 {
		t.Errorf("незнакомое соединение: статус %d, ожидался 404", code)
	}
	code, first := login(map[string]any{"pid": pid, "name": "Лена"})
	if code != 200 || first.UID == "" || first.Secret == "" || first.Name != "Лена" {
		t.Fatalf("регистрация: %d %+v", code, first)
	}
	if code, again := login(map[string]any{"pid": pid, "uid": first.UID, "secret": first.Secret}); code != 200 || again.UID != first.UID || again.Secret != "" {
		t.Errorf("повторный вход: %d %+v (секрет выдаётся только при регистрации)", code, again)
	}
	if code, _ := login(map[string]any{"pid": pid, "uid": first.UID, "secret": "wrong"}); code != 401 {
		t.Errorf("чужой секрет: статус %d, ожидался 401", code)
	}

	code, body := get(t, ts.URL+"/api/profile?uid="+first.UID)
	if code != 200 || !strings.Contains(body, `"name":"Лена"`) || strings.Contains(body, first.Secret) {
		t.Errorf("профиль: %d %s", code, body)
	}
	if code, _ := get(t, ts.URL+"/api/profile?uid=nobody"); code != 404 {
		t.Errorf("профиль несуществующего игрока: статус %d", code)
	}
	if code, body := get(t, ts.URL+"/api/top"); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Errorf("топ без рейтинговых игр: %d %s", code, body)
	}
	if code, body := get(t, ts.URL+"/api/info"); code != 200 || !strings.Contains(body, `"profiles":true`) {
		t.Errorf("info с базой: %d %s", code, body)
	}
}

func TestHTTPSecondTabReplacesFirst(t *testing.T) {
	ts, _ := testServer(t)
	first := openSSE(t, ts.URL, "player-http-2", "Тест")
	first.waitFor(t, func(ClientState) bool { return true })
	second := openSSE(t, ts.URL, "player-http-2", "Тест")
	second.waitFor(t, func(st ClientState) bool { return st.Screen == "menu" })

	select {
	case ev := <-first.events:
		if ev != "replaced" {
			t.Errorf("событие %q", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("первая вкладка не получила replaced")
	}
}
