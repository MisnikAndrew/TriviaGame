package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"time"
)

const (
	sseWriteTimeout  = 10 * time.Second // a stalled client must not hold its stream goroutine forever
	bodyReadTimeout  = 5 * time.Second  // same for a client that trickles a POST body
	defaultMaxPerIP  = 30               // open event streams from one address
	defaultMaxPlayer = 1000             // players on the server at once
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// Server exposes the hub over HTTP: one SSE stream per browser for pushing
// state down, plain JSON POSTs for the player's actions.
type Server struct {
	hub  *Hub
	cfg  Config
	bank *Bank
	urls []string // addresses other devices on the local network can use, for share links
	web  fs.FS

	// Protection for a server that is reachable from the internet. The zero
	// value switches it all off (tests); main turns it on with protect().
	publicURL  string // https://quiz.example.com: where friends from outside connect
	trustProxy bool   // take the player's address from the tunnel's / proxy's headers
	connLim    *limiter
	actionLim  *limiter
	joinLim    *limiter
	streams    *streamCounter
}

// protect limits how much a single address may do: opening event streams,
// sending actions and — most strictly, room codes are short — guessing rooms.
func (s *Server) protect(maxStreamsPerIP int) {
	s.connLim = newLimiter(1, 20)
	s.actionLim = newLimiter(20, 60)
	s.joinLim = newLimiter(0.5, 10)
	s.streams = newStreamCounter(maxStreamsPerIP)
}

func (s *Server) addr(r *http.Request) string { return limitKey(clientIP(r, s.trustProxy)) }

// guard answers 429 when the caller has used up its lim bucket.
func (s *Server) guard(lim *limiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !lim.allow(s.addr(r)) {
			w.Header().Set("Retry-After", "2")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "слишком много запросов — подождите немного"})
			return
		}
		next(w, r)
	}
}

// request is the body of every POST; each endpoint reads the fields it needs.
type request struct {
	PID   string `json:"pid"`
	Name  string `json:"name"`
	Code  string `json:"code"`
	Index int    `json:"index"`
	Seq   int    `json:"seq"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/events", s.guard(s.connLim, s.events))
	mux.HandleFunc("GET /api/info", s.guard(s.actionLim, s.info))

	mux.HandleFunc("POST /api/name", s.guard(s.actionLim, s.action(func(r request) error { return s.hub.Rename(r.PID, r.Name) })))
	mux.HandleFunc("POST /api/quick", s.guard(s.actionLim, s.action(func(r request) error { return s.hub.Quick(r.PID) })))
	mux.HandleFunc("POST /api/bot", s.guard(s.actionLim, s.action(func(r request) error { return s.hub.Bot(r.PID) })))
	mux.HandleFunc("POST /api/room", s.guard(s.actionLim, s.action(func(r request) error { return s.hub.CreateRoom(r.PID) })))
	mux.HandleFunc("POST /api/join", s.guard(s.joinLim, s.guard(s.actionLim, s.action(func(r request) error { return s.hub.JoinRoom(r.PID, r.Code) }))))
	mux.HandleFunc("POST /api/cancel", s.guard(s.actionLim, s.action(func(r request) error { return s.hub.Cancel(r.PID) })))
	mux.HandleFunc("POST /api/leave", s.guard(s.actionLim, s.action(func(r request) error { return s.hub.Leave(r.PID) })))
	mux.HandleFunc("POST /api/pick", s.guard(s.actionLim, s.action(func(r request) error { return s.hub.Pick(r.PID, r.Index) })))
	mux.HandleFunc("POST /api/answer", s.guard(s.actionLim, s.action(func(r request) error { return s.hub.Answer(r.PID, r.Seq, r.Index) })))
	mux.HandleFunc("POST /api/rematch", s.guard(s.actionLim, s.action(func(r request) error { return s.hub.Rematch(r.PID) })))

	mux.Handle("/", noCache(http.FileServerFS(s.web)))
	return mux
}

func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) action(fn func(request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyReadTimeout))
		r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректный запрос"})
			return
		}
		if err := fn(req); err != nil {
			status := http.StatusConflict
			if errors.Is(err, errUnknown) {
				status = http.StatusNotFound
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	type cat struct {
		CatView
		Count int `json:"count"`
	}
	cats := make([]cat, 0, len(s.bank.Cats))
	total := 0
	for _, c := range s.bank.Cats {
		cats = append(cats, cat{catView(c), len(c.Questions)})
		total += len(c.Questions)
	}
	urls := s.urls
	if !isLocalIP(clientIP(r, s.trustProxy)) {
		urls = nil // the addresses inside the home network are nobody's business on the internet
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"urls":       urls,
		"publicUrl":  s.publicURL,
		"rounds":     s.cfg.Rounds,
		"qPerRound":  s.cfg.QPerRound,
		"questionMs": s.cfg.QuestionTime.Milliseconds(),
		"categories": cats,
		"questions":  total,
	})
}

// events is the server-sent-events stream: the browser gets a "state" event
// whenever what it should show changes, plus a keep-alive comment.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("pid")
	if !validID.MatchString(id) {
		http.Error(w, "bad pid", http.StatusBadRequest)
		return
	}
	if !s.hub.canAccept(id) {
		http.Error(w, "на сервере сейчас слишком много игроков, попробуйте позже", http.StatusServiceUnavailable)
		return
	}
	addr := s.addr(r)
	if !s.streams.acquire(addr) {
		http.Error(w, "слишком много открытых соединений с вашего адреса", http.StatusTooManyRequests)
		return
	}
	defer s.streams.release(addr)

	rc := http.NewResponseController(w)
	send := func(format string, args ...any) error {
		rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return err
		}
		return rc.Flush()
	}
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("X-Accel-Buffering", "no") // nginx and friends must not hold the events back
	w.WriteHeader(http.StatusOK)
	if send("retry: 2000\n\n") != nil {
		return
	}

	c, sub := s.hub.connect(id, r.URL.Query().Get("name"))
	defer s.hub.disconnect(c, sub)

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case <-sub.quit:
			send("event: replaced\ndata: {}\n\n")
			return
		case b := <-sub.ch:
			err = send("event: state\ndata: %s\n\n", b)
		case <-ping.C:
			err = send(": ping\n\n")
		}
		if err != nil {
			return
		}
	}
}
