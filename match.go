package main

import (
	"math"
	"math/rand/v2"
	"time"
)

// Config holds the rules of a match. All durations scale together in tests.
type Config struct {
	Rounds         int           // rounds per match
	QPerRound      int           // questions per round
	QuestionTime   time.Duration // time to answer one question
	PickTime       time.Duration // time to choose a category
	IntroTime      time.Duration // category splash before the first question of a round
	RevealTime     time.Duration // how long the right answer stays on screen
	ReconnectGrace time.Duration // a disconnected player forfeits after this long
	FinalIdle      time.Duration // how long the result screen waits for a rematch
}

func DefaultConfig() Config {
	return Config{
		Rounds:         6,
		QPerRound:      3,
		QuestionTime:   15 * time.Second,
		PickTime:       15 * time.Second,
		IntroTime:      2 * time.Second,
		RevealTime:     2500 * time.Millisecond,
		ReconnectGrace: 25 * time.Second,
		FinalIdle:      10 * time.Minute,
	}
}

type phase string

const (
	phasePick     phase = "pick"     // the chooser picks one of offeredCats categories
	phaseIntro    phase = "intro"    // category splash
	phaseQuestion phase = "question" // both players answer
	phaseReveal   phase = "reveal"   // right answer + what the opponent chose
	phaseFinal    phase = "final"    // result, rematch
)

const (
	basePoints  = 50 // correct answer is worth 50..100 points depending on speed
	bonusPoints = 50
	offeredCats = 5 // sectors of the category wheel
)

type evKind int

const (
	evTimer   evKind = iota // phase timer fired (token must match)
	evPick                  // seat chose category n
	evAnswer                // seat answered question seq with option n
	evConn                  // seat connection changed (flag = connected)
	evForfeit               // disconnect grace period of seat expired (seq = connGen)
	evLeave                 // seat left the match
	evRematch               // seat wants a rematch
	evSync                  // resend current view to seat
)

type event struct {
	kind  evKind
	seat  int
	n     int
	seq   int
	token int
	flag  bool
}

type seat struct {
	cl        *Client // nil for the bot
	name      string
	bot       bool
	accuracy  float64 // bots: chance of a right answer
	connected bool
	connGen   int
	left      bool
	rematch   bool
	score     int
}

type answer struct {
	done   bool
	choice int // -1: no answer (timeout)
	points int
}

type roundLog struct {
	cat *Category
	res [2][]int // per seat, per question: 0 not played, 1 right, 2 wrong
}

type matchHooks struct {
	rematch func(*Match) // both players agreed on a rematch
	ended   func(*Match) // the match goroutine is about to exit
}

// Match is a duel between two seats. Its whole state is owned by the run
// goroutine; the outside world talks to it through post().
type Match struct {
	cfg         Config
	bank        *Bank
	seats       [2]*seat
	firstPicker int
	hooks       matchHooks

	events chan event
	done   chan struct{}

	phase    phase
	token    int // bumped on every phase change, invalidates old timers
	round    int
	qi       int
	chooser  int
	choices  []*Category
	cur      *Category
	qs       []Asked
	seq      int // global question counter, lets us drop stale answers
	deadline time.Time
	ans      [2]answer
	log      []roundLog
	usedCats map[string]bool
	usedQs   map[*Question]bool
	forced   int // seat that won because the opponent left, -1 otherwise
	closed   bool
}

func newMatch(cfg Config, bank *Bank, seats [2]*seat, firstPicker int, hooks matchHooks) *Match {
	m := &Match{
		cfg: cfg, bank: bank, seats: seats, firstPicker: firstPicker, hooks: hooks,
		events:   make(chan event, 64),
		done:     make(chan struct{}),
		usedCats: map[string]bool{},
		usedQs:   map[*Question]bool{},
		forced:   -1,
		log:      make([]roundLog, cfg.Rounds),
	}
	for r := range m.log {
		m.log[r].res = [2][]int{make([]int, cfg.QPerRound), make([]int, cfg.QPerRound)}
	}
	return m
}

// post delivers an event to the match without ever blocking the caller.
func (m *Match) post(ev event) {
	select {
	case m.events <- ev:
	case <-m.done:
	default:
		go func() {
			select {
			case m.events <- ev:
			case <-m.done:
			}
		}()
	}
}

func (m *Match) after(d time.Duration, ev event) {
	time.AfterFunc(d, func() { m.post(ev) })
}

func (m *Match) run() {
	defer func() {
		close(m.done)
		if m.hooks.ended != nil {
			m.hooks.ended(m)
		}
	}()
	m.startPick()
	for !m.closed {
		m.handle(<-m.events)
	}
}

func (m *Match) handle(ev event) {
	switch ev.kind {
	case evTimer:
		if ev.token == m.token {
			m.onTimer()
		}
	case evPick:
		m.onPick(ev)
	case evAnswer:
		m.onAnswer(ev)
	case evConn:
		m.onConn(ev)
	case evForfeit:
		m.onForfeit(ev)
	case evLeave:
		m.onLeave(ev.seat)
	case evRematch:
		m.onRematch(ev.seat)
	case evSync:
		m.sendTo(ev.seat)
	}
}

func (m *Match) onTimer() {
	switch m.phase {
	case phasePick:
		m.choose(rand.IntN(len(m.choices)))
	case phaseIntro:
		m.startQuestion()
	case phaseQuestion:
		m.reveal()
	case phaseReveal:
		m.advance()
	case phaseFinal:
		m.closed = true // nobody asked for a rematch in time
	}
}

// ---- round flow ----------------------------------------------------------

func (m *Match) startPick() {
	m.phase = phasePick
	m.token++
	m.chooser = (m.firstPicker + m.round) % 2
	m.choices = m.offerCategories(offeredCats)
	m.deadline = time.Now().Add(m.cfg.PickTime)
	m.broadcast()
	m.after(m.cfg.PickTime, event{kind: evTimer, token: m.token})
	if m.seats[m.chooser].bot {
		m.after(botDelay(m.cfg.PickTime, 0.08, 0.25), event{
			kind: evPick, seat: m.chooser, n: rand.IntN(len(m.choices)), token: m.token,
		})
	}
}

func (m *Match) onPick(ev event) {
	if m.phase != phasePick || ev.seat != m.chooser || ev.n < 0 || ev.n >= len(m.choices) {
		return
	}
	if ev.token != 0 && ev.token != m.token {
		return
	}
	m.choose(ev.n)
}

func (m *Match) choose(i int) {
	m.cur = m.choices[i]
	m.usedCats[m.cur.ID] = true
	m.log[m.round].cat = m.cur
	m.qs = m.drawQuestions(m.cur)
	m.qi = 0

	m.phase = phaseIntro
	m.token++
	m.deadline = time.Now().Add(m.cfg.IntroTime)
	m.broadcast()
	m.after(m.cfg.IntroTime, event{kind: evTimer, token: m.token})
}

func (m *Match) startQuestion() {
	m.phase = phaseQuestion
	m.token++
	m.seq++
	m.ans = [2]answer{}
	m.deadline = time.Now().Add(m.cfg.QuestionTime)
	m.broadcast()
	m.after(m.cfg.QuestionTime, event{kind: evTimer, token: m.token})

	for i, s := range m.seats {
		if s.bot {
			m.after(botDelay(m.cfg.QuestionTime, 0.12, 0.65), event{
				kind: evAnswer, seat: i, seq: m.seq, n: s.pickOption(m.qs[m.qi].Answer),
			})
		}
	}
}

func (m *Match) onAnswer(ev event) {
	if m.phase != phaseQuestion || ev.seq != m.seq || ev.n < 0 || ev.n >= len(m.qs[m.qi].Options) {
		return
	}
	if ev.seat < 0 || ev.seat > 1 || m.ans[ev.seat].done {
		return
	}
	a := answer{done: true, choice: ev.n}
	if ev.n == m.qs[m.qi].Answer {
		a.points = m.points(time.Until(m.deadline))
	}
	m.ans[ev.seat] = a
	if m.allAnswered() {
		m.reveal()
		return
	}
	m.broadcast() // lets the opponent see "answered"
}

// allAnswered reports whether nobody else can answer: humans that dropped
// off the connection are not waited for.
func (m *Match) allAnswered() bool {
	for i, s := range m.seats {
		if m.ans[i].done {
			continue
		}
		if s.bot || (s.connected && !s.left) {
			return false
		}
	}
	return true
}

func (m *Match) points(remaining time.Duration) int {
	frac := float64(remaining) / float64(m.cfg.QuestionTime)
	frac = math.Max(0, math.Min(1, frac))
	p := basePoints + int(math.Round(bonusPoints*frac))
	if m.round == m.cfg.Rounds-1 {
		p *= 2 // the last round is worth double
	}
	return p
}

func (m *Match) reveal() {
	m.phase = phaseReveal
	m.token++
	for i, s := range m.seats {
		if !m.ans[i].done {
			m.ans[i] = answer{done: true, choice: -1}
		}
		s.score += m.ans[i].points
		res := 2
		if m.ans[i].choice == m.qs[m.qi].Answer {
			res = 1
		}
		m.log[m.round].res[i][m.qi] = res
	}
	m.broadcast()
	m.after(m.cfg.RevealTime, event{kind: evTimer, token: m.token})
}

func (m *Match) advance() {
	switch {
	case m.qi+1 < m.cfg.QPerRound:
		m.qi++
		m.startQuestion()
	case m.round+1 < m.cfg.Rounds:
		m.round++
		m.startPick()
	default:
		m.finish()
	}
}

func (m *Match) finish() {
	m.phase = phaseFinal
	m.token++
	m.broadcast()
	m.after(m.cfg.FinalIdle, event{kind: evTimer, token: m.token})
}

// ---- players coming and going --------------------------------------------

func (m *Match) onConn(ev event) {
	s := m.seats[ev.seat]
	if s.bot || s.left {
		return
	}
	s.connGen++
	s.connected = ev.flag
	if ev.flag {
		m.broadcast()
		return
	}
	m.after(m.cfg.ReconnectGrace, event{kind: evForfeit, seat: ev.seat, seq: s.connGen})
	if m.phase == phaseQuestion && m.allAnswered() {
		m.reveal()
		return
	}
	m.broadcast()
}

func (m *Match) onForfeit(ev event) {
	s := m.seats[ev.seat]
	if s.left || s.connected || s.connGen != ev.seq {
		return
	}
	m.onLeave(ev.seat) // a long disconnect counts as leaving
}

func (m *Match) onLeave(i int) {
	s, other := m.seats[i], m.seats[1-i]
	if s.left {
		return
	}
	s.left = true
	if other.bot || other.left {
		m.closed = true
		return
	}
	if m.phase != phaseFinal {
		m.forced = 1 - i // the one who stayed wins
		m.finish()
		return
	}
	m.broadcast() // the opponent sees that the rematch is off
}

func (m *Match) onRematch(i int) {
	s, other := m.seats[i], m.seats[1-i]
	if m.phase != phaseFinal || s.left || other.left {
		return
	}
	s.rematch = true
	if other.bot {
		other.rematch = true
	}
	if s.rematch && other.rematch {
		if m.hooks.rematch != nil {
			m.hooks.rematch(m)
		}
		m.closed = true
		return
	}
	m.broadcast()
}

// ---- questions -----------------------------------------------------------

func (m *Match) eligible() []*Category {
	var pool []*Category
	for _, c := range m.bank.Cats {
		if !m.usedCats[c.ID] && len(m.freeQuestions(c)) >= m.cfg.QPerRound {
			pool = append(pool, c)
		}
	}
	return pool
}

func (m *Match) freeQuestions(c *Category) []*Question {
	var free []*Question
	for i := range c.Questions {
		if q := &c.Questions[i]; !m.usedQs[q] {
			free = append(free, q)
		}
	}
	return free
}

// offerCategories returns n random categories that were not played in this
// match yet, relaxing the restrictions only when the bank runs dry.
func (m *Match) offerCategories(n int) []*Category {
	pool := m.eligible()
	if len(pool) < n {
		clear(m.usedCats)
		pool = m.eligible()
	}
	if len(pool) < n {
		clear(m.usedQs)
		pool = m.eligible()
	}
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if len(pool) > n {
		pool = pool[:n]
	}
	return pool
}

func (m *Match) drawQuestions(c *Category) []Asked {
	free := m.freeQuestions(c)
	if len(free) < m.cfg.QPerRound {
		clear(m.usedQs)
		free = m.freeQuestions(c)
	}
	rand.Shuffle(len(free), func(i, j int) { free[i], free[j] = free[j], free[i] })
	qs := make([]Asked, 0, m.cfg.QPerRound)
	for _, q := range free[:m.cfg.QPerRound] {
		m.usedQs[q] = true
		qs = append(qs, newAsked(q))
	}
	return qs
}

// ---- views ---------------------------------------------------------------

func (m *Match) broadcast() {
	for i := range m.seats {
		m.sendTo(i)
	}
}

func (m *Match) sendTo(i int) {
	s := m.seats[i]
	if s.cl == nil || s.left {
		return
	}
	s.cl.push(ClientState{Screen: "game", Name: s.name, Game: m.viewFor(i)})
}

func (m *Match) viewFor(i int) *GameView {
	me, op := m.seats[i], m.seats[1-i]
	g := &GameView{
		Phase:     string(m.phase),
		Round:     m.round + 1,
		Rounds:    m.cfg.Rounds,
		QIndex:    m.qi + 1,
		QPerRound: m.cfg.QPerRound,
		Double:    m.round == m.cfg.Rounds-1,
		Me:        PlayerView{Name: me.name, Score: me.score, Connected: true},
		Opp:       PlayerView{Name: op.name, Score: op.score, Connected: op.bot || (op.connected && !op.left), Bot: op.bot},
		MyAnswer:  -1,
		OppAnswer: -1,
		Correct:   -1,
	}
	for r := range m.log {
		rv := RoundView{Me: m.log[r].res[i], Opp: m.log[r].res[1-i]}
		if c := m.log[r].cat; c != nil {
			cv := catView(c)
			rv.Cat = &cv
		}
		g.Board = append(g.Board, rv)
	}

	switch m.phase {
	case phasePick:
		g.IPick = m.chooser == i
		g.ChooserName = m.seats[m.chooser].name
		for _, c := range m.choices {
			g.Choices = append(g.Choices, catView(c))
		}
		g.setTimer(m.deadline, m.cfg.PickTime)
	case phaseIntro:
		cv := catView(m.cur)
		g.Cat = &cv
	case phaseQuestion, phaseReveal:
		cv := catView(m.cur)
		g.Cat = &cv
		q := m.qs[m.qi]
		g.Question = &QuestionView{Seq: m.seq, Text: q.Text, Options: q.Options}
		g.MyAnswer = m.ans[i].choice
		if !m.ans[i].done {
			g.MyAnswer = -1
		}
		g.OppAnswered = m.ans[1-i].done
		if m.phase == phaseQuestion {
			g.setTimer(m.deadline, m.cfg.QuestionTime)
		} else {
			g.Correct = q.Answer
			g.OppAnswer = m.ans[1-i].choice
			g.MyPoints = m.ans[i].points
			g.OppPoints = m.ans[1-i].points
		}
	case phaseFinal:
		g.Round = m.cfg.Rounds
		g.Result = m.resultFor(i)
		g.Forfeit = m.forced >= 0
		g.OppLeft = op.left
		g.RematchMe = me.rematch
		g.RematchOpp = op.rematch
	}
	return g
}

func (g *GameView) setTimer(deadline time.Time, total time.Duration) {
	g.TimeLeftMs = max(0, time.Until(deadline).Milliseconds())
	g.TimeTotalMs = total.Milliseconds()
}

func (m *Match) resultFor(i int) string {
	if m.forced >= 0 {
		if m.forced == i {
			return "win"
		}
		return "lose"
	}
	a, b := m.seats[i].score, m.seats[1-i].score
	switch {
	case a > b:
		return "win"
	case a < b:
		return "lose"
	}
	return "draw"
}

func catView(c *Category) CatView { return CatView{ID: c.ID, Name: c.Name, Icon: c.Icon} }
