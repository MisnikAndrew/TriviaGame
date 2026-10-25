package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

type clientStatus string

const (
	stMenu  clientStatus = "menu"
	stQueue clientStatus = "queue" // waiting for a random opponent
	stRoom  clientStatus = "room"  // waiting for a friend in a private room
	stMatch clientStatus = "match"
)

const (
	roomCodeLen      = 4
	roomCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I
	waitGrace        = 30 * time.Second                   // queue/room survive a short disconnect
	idleClientTTL    = 3 * time.Minute                    // a forgotten menu player just gets a fresh Client on reconnect
)

var (
	errUnknown = errors.New("игрок не найден, обновите страницу")
	errBusy    = errors.New("сначала завершите текущую игру")
	errNoRoom  = errors.New("комната не найдена или уже закрыта")
	errOwnRoom = errors.New("это ваша собственная комната")
	errNoGame  = errors.New("вы сейчас не в игре")
)

// sub is one open SSE connection.
type sub struct {
	ch   chan []byte
	quit chan struct{} // closed when a newer connection of the same player replaces this one
	once sync.Once
}

func newSub() *sub { return &sub{ch: make(chan []byte, 32), quit: make(chan struct{})} }

// send never blocks: states are full snapshots, so when a slow reader falls
// behind the oldest one is dropped.
func (s *sub) send(b []byte) {
	select {
	case s.ch <- b:
		return
	default:
	}
	select {
	case <-s.ch:
	default:
	}
	select {
	case s.ch <- b:
	default:
	}
}

func (s *sub) replace() { s.once.Do(func() { close(s.quit) }) }

// Client is a browser identified by the id it keeps in sessionStorage.
type Client struct {
	id string

	// guarded by Hub.mu
	name     string
	status   clientStatus
	room     string
	match    *Match
	seat     int
	connGen  int
	lastSeen time.Time

	subMu sync.Mutex
	sub   *sub
}

func (c *Client) push(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.subMu.Lock()
	s := c.sub
	c.subMu.Unlock()
	if s != nil {
		s.send(b)
	}
}

func (c *Client) connected() bool {
	c.subMu.Lock()
	defer c.subMu.Unlock()
	return c.sub != nil
}

// Hub owns clients, the matchmaking queue and private rooms. Lock order:
// Hub.mu, then Client.subMu. Matches only ever take Client.subMu (via push)
// and, from their own goroutine, Hub.mu inside the hooks; the hub never
// blocks on a match, so there is no cycle.
type Hub struct {
	cfg  Config
	bank *Bank

	maxClients int // 0: no limit

	mu      sync.Mutex
	clients map[string]*Client
	queue   string            // id of the player waiting for a random opponent
	rooms   map[string]string // room code -> owner id
}

func NewHub(cfg Config, bank *Bank) *Hub {
	return &Hub{cfg: cfg, bank: bank, clients: map[string]*Client{}, rooms: map[string]string{}}
}

// canAccept reports whether the player may open a connection: a known one
// always may, a new one only while the server has room.
func (h *Hub) canAccept(id string) bool {
	if h.maxClients <= 0 {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, known := h.clients[id]
	return known || len(h.clients) < h.maxClients
}

// Janitor forgets players that left the menu long ago.
func (h *Hub) Janitor(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.mu.Lock()
			for id, c := range h.clients {
				if c.status == stMenu && !c.connected() && time.Since(c.lastSeen) > idleClientTTL {
					delete(h.clients, id)
				}
			}
			h.mu.Unlock()
		}
	}
}

func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 16 {
		s = string([]rune(s)[:16])
	}
	if s == "" {
		s = "Игрок"
	}
	return s
}

// ---- connections ---------------------------------------------------------

func (h *Hub) connect(id, name string) (*Client, *sub) {
	h.mu.Lock()
	c := h.clients[id]
	if c == nil {
		c = &Client{id: id, name: cleanName(name), status: stMenu}
		h.clients[id] = c
	}
	c.lastSeen = time.Now()
	c.connGen++

	s := newSub()
	c.subMu.Lock()
	old := c.sub
	c.sub = s
	c.subMu.Unlock()
	if old != nil {
		old.replace()
	}

	m, seat := c.match, c.seat
	h.pushState(c)
	h.mu.Unlock()

	if m != nil {
		m.post(event{kind: evConn, seat: seat, flag: true})
	}
	return c, s
}

func (h *Hub) disconnect(c *Client, s *sub) {
	c.subMu.Lock()
	if c.sub != s { // already replaced by a newer connection
		c.subMu.Unlock()
		return
	}
	c.sub = nil
	c.subMu.Unlock()

	h.mu.Lock()
	c.lastSeen = time.Now()
	c.connGen++
	gen := c.connGen
	m, seat, status := c.match, c.seat, c.status
	h.mu.Unlock()

	switch status {
	case stMatch:
		if m != nil {
			m.post(event{kind: evConn, seat: seat, flag: false})
		}
	case stQueue, stRoom:
		time.AfterFunc(waitGrace, func() { h.expireWaiting(c, gen) })
	}
}

func (h *Hub) expireWaiting(c *Client, gen int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.connGen != gen || c.connected() {
		return // came back in time
	}
	if c.status == stQueue || c.status == stRoom {
		h.stopWaiting(c)
	}
}

// pushState sends the non-game screens; during a match the match itself
// talks to the client. Caller holds h.mu.
func (h *Hub) pushState(c *Client) {
	st := ClientState{Name: c.name}
	switch c.status {
	case stMatch:
		return
	case stQueue:
		st.Screen = "queue"
	case stRoom:
		st.Screen = "room"
		st.Room = c.room
	default:
		st.Screen = "menu"
	}
	c.push(st)
}

// stopWaiting drops c out of the queue or its private room. Caller holds h.mu.
func (h *Hub) stopWaiting(c *Client) {
	if h.queue == c.id {
		h.queue = ""
	}
	if c.room != "" {
		delete(h.rooms, c.room)
		c.room = ""
	}
	c.status = stMenu
	h.pushState(c)
}

func (h *Hub) get(id string) (*Client, error) {
	c := h.clients[id]
	if c == nil {
		return nil, errUnknown
	}
	c.lastSeen = time.Now()
	return c, nil
}

// ---- lobby actions -------------------------------------------------------

func (h *Hub) Rename(id, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	if c.status == stMatch {
		return errBusy
	}
	c.name = cleanName(name)
	h.pushState(c)
	return nil
}

func (h *Hub) Quick(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	if c.status != stMenu {
		return errBusy
	}
	if w := h.clients[h.queue]; w != nil && w != c && w.status == stQueue && w.connected() {
		h.queue = ""
		h.startMatch(w, c)
		return nil
	}
	h.queue = id
	c.status = stQueue
	h.pushState(c)
	return nil
}

func (h *Hub) Bot(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	if c.status != stMenu {
		return errBusy
	}
	h.startMatch(c, nil)
	return nil
}

func (h *Hub) CreateRoom(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	if c.status != stMenu {
		return errBusy
	}
	for {
		code := make([]byte, roomCodeLen)
		for i := range code {
			code[i] = roomCodeAlphabet[rand.IntN(len(roomCodeAlphabet))]
		}
		if _, taken := h.rooms[string(code)]; !taken {
			c.room = string(code)
			break
		}
	}
	h.rooms[c.room] = id
	c.status = stRoom
	h.pushState(c)
	return nil
}

func (h *Hub) JoinRoom(id, code string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	if c.status != stMenu {
		return errBusy
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	owner := h.clients[h.rooms[code]]
	if owner == nil || owner.status != stRoom || owner.room != code || !owner.connected() {
		return errNoRoom
	}
	if owner == c {
		return errOwnRoom
	}
	delete(h.rooms, code)
	owner.room = ""
	h.startMatch(owner, c)
	return nil
}

// Cancel leaves the queue or closes the private room.
func (h *Hub) Cancel(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	if c.status == stQueue || c.status == stRoom {
		h.stopWaiting(c)
	}
	return nil
}

// Leave abandons the current match (a forfeit while it is still running) and
// returns to the menu.
func (h *Hub) Leave(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	switch c.status {
	case stQueue, stRoom:
		h.stopWaiting(c)
	case stMatch:
		m, seat := c.match, c.seat
		c.match, c.status = nil, stMenu
		if m != nil {
			m.post(event{kind: evLeave, seat: seat})
		}
		h.pushState(c)
	}
	return nil
}

// ---- in-match actions ----------------------------------------------------

func (h *Hub) matchOf(id string) (*Match, int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return nil, 0, err
	}
	if c.match == nil {
		return nil, 0, errNoGame
	}
	return c.match, c.seat, nil
}

func (h *Hub) Pick(id string, index int) error {
	m, seat, err := h.matchOf(id)
	if err != nil {
		return err
	}
	m.post(event{kind: evPick, seat: seat, n: index})
	return nil
}

func (h *Hub) Answer(id string, seq, index int) error {
	m, seat, err := h.matchOf(id)
	if err != nil {
		return err
	}
	m.post(event{kind: evAnswer, seat: seat, seq: seq, n: index})
	return nil
}

func (h *Hub) Rematch(id string) error {
	m, seat, err := h.matchOf(id)
	if err != nil {
		return err
	}
	m.post(event{kind: evRematch, seat: seat})
	return nil
}

// ---- match lifecycle -----------------------------------------------------

// startMatch pairs a with b, or with a bot when b is nil. Caller holds h.mu.
func (h *Hub) startMatch(a, b *Client) {
	seats := [2]*seat{h.humanSeat(a), nil}
	if b != nil {
		seats[1] = h.humanSeat(b)
	} else {
		seats[1] = newBotSeat()
	}
	h.launch(seats, rand.IntN(2))
}

func (h *Hub) humanSeat(c *Client) *seat {
	return &seat{cl: c, name: c.name, connected: c.connected()}
}

func (h *Hub) launch(seats [2]*seat, firstPicker int) {
	m := newMatch(h.cfg, h.bank, seats, firstPicker, matchHooks{rematch: h.rematchHook, ended: h.endedHook})
	for i, s := range seats {
		if c := s.cl; c != nil {
			c.status, c.match, c.seat, c.room = stMatch, m, i, ""
		}
	}
	go m.run()
}

// rematchHook runs on the finished match's goroutine once both players want
// to play again: same people, same bot, the other one picks first.
func (h *Hub) rematchHook(old *Match) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var seats [2]*seat
	for i, s := range old.seats {
		if s.cl != nil && s.cl.match != old {
			return // that player already went elsewhere
		}
		seats[i] = &seat{cl: s.cl, name: s.name, bot: s.bot, accuracy: s.accuracy, connected: s.bot || s.cl.connected()}
	}
	h.launch(seats, 1-old.firstPicker)
}

// endedHook sends everybody still attached to a finished match back to the menu.
func (h *Hub) endedHook(m *Match) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range m.seats {
		if c := s.cl; c != nil && c.match == m {
			c.match, c.status = nil, stMenu
			h.pushState(c)
		}
	}
}
