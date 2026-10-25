package main

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
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

	minQuestionSecs = 3   // the shortest time to answer a question anybody may ask for
	maxQuestionSecs = 120 // ...and the longest one a game may choose
	maxRounds       = 12
	maxQPerRound    = 10
)

var (
	errUnknown    = errors.New("игрок не найден, обновите страницу")
	errBusy       = errors.New("сначала завершите текущую игру")
	errNoRoom     = errors.New("комната не найдена или уже закрыта")
	errNoGame     = errors.New("вы сейчас не в игре")
	errBadTime    = errors.New("время на ответ должно быть от 3 до 120 секунд")
	errBadSize    = errors.New("в матче должно быть от 1 до 12 раундов и от 1 до 10 вопросов в раунде")
	errBadCats    = errors.New("выберите минимум две темы, в которых хватает вопросов")
	errBadBot     = errors.New("неизвестная сложность бота")
	errBadBots    = errors.New("ботов может быть от 0 до 7, а игроков вместе с ними — не больше 8")
	errRoomFull   = errors.New("в комнате нет свободных мест")
	errNotHost    = errors.New("начать игру может только создатель комнаты")
	errFewPlayers = errors.New("для игры нужны минимум два участника: добавьте друга или ботов")
	errBadHint    = errors.New("неизвестная подсказка")
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

	user string // the profile this browser plays for, "" if none (guarded by Hub.mu)

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
	cfg   Config
	bank  *Bank
	store *Store // nil: no profiles and no statistics

	maxClients int // 0: no limit

	mu      sync.Mutex
	clients map[string]*Client
	queue   string           // id of the player waiting for a random opponent
	rooms   map[string]*room // by code
}

// room is a private room before its game starts: the creator and the friends
// who joined, in the order they came.
type room struct {
	code    string
	members []*Client // the creator first
	rules   ruleset
}

func NewHub(cfg Config, bank *Bank) *Hub {
	return &Hub{cfg: cfg, bank: bank, clients: map[string]*Client{}, rooms: map[string]*room{}}
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
	if status == stRoom {
		h.pushRoom(c.room) // the others see who is away
	}
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
		rm := h.rooms[c.room]
		if rm == nil {
			st.Screen = "menu"
			break
		}
		st.Screen = "room"
		st.Room = rm.code
		r := rulesView(rm.rules.cfg)
		st.Rules = &r
		if len(rm.rules.bank.Cats) < len(h.bank.Cats) {
			for _, cat := range rm.rules.bank.Cats {
				st.RoomCats = append(st.RoomCats, catView(cat))
			}
		}
		st.Lobby = rm.lobbyFor(c)
	default:
		st.Screen = "menu"
	}
	c.push(st)
}

// pushRoom sends everybody in the room the list of who is there. Caller holds h.mu.
func (h *Hub) pushRoom(code string) {
	if rm := h.rooms[code]; rm != nil {
		for _, m := range rm.members {
			h.pushState(m)
		}
	}
}

func (rm *room) lobbyFor(c *Client) *LobbyView {
	lv := &LobbyView{Host: rm.members[0] == c, Max: maxSeats}
	here := 0
	for i, m := range rm.members {
		lv.Players = append(lv.Players, LobbyPlayer{Name: m.name, Owner: i == 0, Me: m == c, Connected: m.connected()})
		if m.connected() {
			here++
		}
	}
	for range rm.rules.bots {
		lv.Players = append(lv.Players, LobbyPlayer{Name: "🤖 Бот", Bot: true, Connected: true})
	}
	lv.CanStart = lv.Host && here+rm.rules.bots >= 2
	return lv
}

// stopWaiting drops c out of the queue or its private room: the creator
// closes the room, a guest just leaves it. Caller holds h.mu.
func (h *Hub) stopWaiting(c *Client) {
	if h.queue == c.id {
		h.queue = ""
	}
	rm := h.rooms[c.room]
	c.room = ""
	c.status = stMenu
	if rm != nil {
		if rm.members[0] == c {
			delete(h.rooms, rm.code)
			for _, m := range rm.members[1:] {
				m.room, m.status = "", stMenu
				h.pushState(m)
			}
		} else {
			rm.members = slices.DeleteFunc(rm.members, func(m *Client) bool { return m == c })
			h.pushRoom(rm.code)
		}
	}
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
	if c.user != "" {
		h.store.SetName(c.user, c.name)
	}
	h.pushState(c)
	return nil
}

// known reports whether the player has a connection with the server.
func (h *Hub) known(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clients[id] != nil
}

// Attach makes the browser play for the profile uid, whose name it takes.
func (h *Hub) Attach(id, uid, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	c.user = uid
	if c.status != stMatch {
		c.name = name
		h.pushState(c)
	}
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
		h.startMatch([]*Client{w, c}, h.defaultRules())
		return nil
	}
	h.queue = id
	c.status = stQueue
	h.pushState(c)
	return nil
}

// Bot starts a game against bots (one, unless the player asked for more).
func (h *Hub) Bot(id string, o GameOpts) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	if c.status != stMenu {
		return errBusy
	}
	rs, err := h.rules(o)
	if err != nil {
		return err
	}
	rs.bots = max(rs.bots, 1)
	h.startMatch([]*Client{c}, rs)
	return nil
}

// GameOpts are the rules the creator of a private room or of a game against a
// bot chooses; the zero value means the server's defaults.
type GameOpts struct {
	Seconds   int      // time to answer one question
	Rounds    int      // rounds in the match
	QPerRound int      // questions in a round
	Speed     bool     // a faster right answer is worth more
	Tiebreak  bool     // level scores are settled by extra questions
	Hints     bool     // every player may use each hint once
	Bots      int      // bots that play too
	Cats      []string // ids of the categories to play, none: all of them
	Level     string   // bots: easy | normal | hard
}

// ruleset is everything that makes one match different from another.
type ruleset struct {
	cfg  Config
	bank *Bank
	bot  botLevel // how well the bots play
	bots int      // how many of them
}

func (h *Hub) defaultRules() ruleset { return ruleset{cfg: h.cfg, bank: h.bank, bot: botNormal} }

// rules checks what a player asked for and turns it into the rules of a match.
func (h *Hub) rules(o GameOpts) (ruleset, error) {
	rs := h.defaultRules()
	if o.Seconds != 0 {
		if o.Seconds < minQuestionSecs || o.Seconds > maxQuestionSecs {
			return rs, errBadTime
		}
		rs.cfg.QuestionTime = time.Duration(o.Seconds) * time.Second
	}
	if o.Rounds != 0 {
		rs.cfg.Rounds = o.Rounds
	}
	if o.QPerRound != 0 {
		rs.cfg.QPerRound = o.QPerRound
	}
	if rs.cfg.Rounds < 1 || rs.cfg.Rounds > maxRounds || rs.cfg.QPerRound < 1 || rs.cfg.QPerRound > maxQPerRound {
		return rs, errBadSize
	}
	if o.Bots < 0 || o.Bots > maxSeats-1 {
		return rs, errBadBots
	}
	rs.bots = o.Bots
	rs.cfg.SpeedScore = o.Speed
	rs.cfg.Tiebreak = o.Tiebreak
	rs.cfg.Hints = o.Hints
	if len(o.Cats) > 0 {
		bank, err := h.bank.Only(rs.cfg.QPerRound, o.Cats)
		if err != nil {
			return rs, err
		}
		rs.bank = bank
	} else if rs.cfg.QPerRound != h.cfg.QPerRound {
		rs.bank = h.bank.Without(rs.cfg.QPerRound) // categories too small for the longer round drop out
	}
	if len(rs.bank.Cats) < 2 {
		return rs, errBadCats
	}
	if o.Level != "" {
		l, ok := botLevels[o.Level]
		if !ok {
			return rs, errBadBot
		}
		rs.bot = l
	}
	return rs, nil
}

func (h *Hub) CreateRoom(id string, o GameOpts) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	if c.status != stMenu {
		return errBusy
	}
	rs, err := h.rules(o)
	if err != nil {
		return err
	}
	rm := &room{members: []*Client{c}, rules: rs}
	for {
		code := make([]byte, roomCodeLen)
		for i := range code {
			code[i] = roomCodeAlphabet[rand.IntN(len(roomCodeAlphabet))]
		}
		if _, taken := h.rooms[string(code)]; !taken {
			rm.code = string(code)
			break
		}
	}
	h.rooms[rm.code] = rm
	c.room = rm.code
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
	rm := h.rooms[strings.ToUpper(strings.TrimSpace(code))]
	if rm == nil || !rm.members[0].connected() {
		return errNoRoom
	}
	if len(rm.members)+rm.rules.bots >= maxSeats {
		return errRoomFull
	}
	rm.members = append(rm.members, c)
	c.room, c.status = rm.code, stRoom
	h.pushRoom(rm.code)
	return nil
}

// Start begins the game of the room: its creator presses the button once
// everybody is here. Friends who dropped off in the meantime are left out.
func (h *Hub) Start(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, err := h.get(id)
	if err != nil {
		return err
	}
	rm := h.rooms[c.room]
	if c.status != stRoom || rm == nil || rm.members[0] != c {
		return errNotHost
	}
	var here []*Client
	for _, m := range rm.members {
		if m.connected() {
			here = append(here, m)
		}
	}
	if len(here)+rm.rules.bots < 2 {
		return errFewPlayers
	}
	delete(h.rooms, rm.code)
	for _, m := range rm.members {
		m.room = ""
		if !m.connected() {
			m.status = stMenu
			h.pushState(m)
		}
	}
	h.startMatch(here, rm.rules)
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

// Pause freezes (on) or unfreezes the category wheel; the match ignores it in any other phase.
func (h *Hub) Pause(id string, on bool) error {
	m, seat, err := h.matchOf(id)
	if err != nil {
		return err
	}
	m.post(event{kind: evPause, seat: seat, flag: on})
	return nil
}

// Hint spends one of the player's hints: "fifty", "second" or "extra".
func (h *Hub) Hint(id, kind string) error {
	n := slices.Index([]string{"fifty", "second", "extra"}, kind)
	if n < 0 {
		return errBadHint
	}
	m, seat, err := h.matchOf(id)
	if err != nil {
		return err
	}
	m.post(event{kind: evHint, seat: seat, n: n})
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

// startMatch seats the humans (and the bots the rules ask for) at one table.
// Caller holds h.mu.
func (h *Hub) startMatch(humans []*Client, rs ruleset) {
	seats := make([]*seat, 0, len(humans)+rs.bots)
	var taken []string
	for _, c := range humans {
		seats = append(seats, h.humanSeat(c))
		taken = append(taken, c.name)
	}
	seats = append(seats, newBotSeats(rs.bots, rs.bot, taken)...)
	labelDuplicates(seats)
	unlinkSameUser(seats)
	h.launch(seats, rand.IntN(len(seats)), rs)
}

// unlinkSameUser keeps a profile from playing against itself (two tabs of one
// browser): such a match counts for nobody.
func unlinkSameUser(seats []*seat) {
	n := map[string]int{}
	for _, s := range seats {
		n[s.user]++
	}
	for _, s := range seats {
		if s.user != "" && n[s.user] > 1 {
			s.user = ""
		}
	}
}

// labelDuplicates tells apart players who have the same name.
func labelDuplicates(seats []*seat) {
	seen := map[string]int{}
	for _, s := range seats {
		seen[s.name]++
		if n := seen[s.name]; n > 1 {
			s.label = s.name + " " + strconv.Itoa(n)
		}
	}
}

func (h *Hub) humanSeat(c *Client) *seat {
	return &seat{cl: c, user: c.user, name: c.name, connected: c.connected()}
}

func (h *Hub) launch(seats []*seat, firstPicker int, rs ruleset) {
	hooks := matchHooks{rematch: h.rematchHook, ended: h.endedHook}
	if h.store != nil {
		hooks.answers, hooks.finished = h.store.RecordAnswers, h.store.RecordMatch
	}
	m := newMatch(rs.cfg, rs.bank, seats, firstPicker, hooks)
	for i, s := range seats {
		if c := s.cl; c != nil {
			c.status, c.match, c.seat, c.room = stMatch, m, i, ""
		}
	}
	go m.run()
}

// rematchHook runs on the finished match's goroutine once everybody who is
// still there wants to play again: same people, same bots, same rules, the
// next one picks first.
func (h *Hub) rematchHook(old *Match) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var seats []*seat
	for _, s := range old.seats {
		if s.left || (s.cl != nil && s.cl.match != old) { // gone, or already somewhere else
			continue
		}
		seats = append(seats, &seat{
			cl: s.cl, user: s.user, name: s.name, label: s.label, bot: s.bot, accuracy: s.accuracy, delayLo: s.delayLo, delayHi: s.delayHi,
			connected: s.bot || s.cl.connected(),
		})
	}
	if len(seats) < 2 {
		return
	}
	h.launch(seats, (old.firstPicker+1)%len(seats), ruleset{cfg: old.cfg, bank: old.bank})
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
