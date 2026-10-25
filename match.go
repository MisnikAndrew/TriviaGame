package main

import (
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"time"
)

// Config holds the rules of a match. All durations scale together in tests.
type Config struct {
	Rounds         int           // rounds per match
	QPerRound      int           // questions per round
	QuestionTime   time.Duration // time to answer one question
	SpeedScore     bool          // a right answer is worth more the sooner it comes in (minCorrectPoints..correctPoints)
	Tiebreak       bool          // level scores are settled by extra questions instead of a draw
	Hints          bool          // every player may use each hint once per match
	PickTime       time.Duration // time to choose a category
	PauseMax       time.Duration // a pause on the category wheel ends by itself after this long (0: never)
	IntroTime      time.Duration // category splash before the first question of a round
	RevealTime     time.Duration // how long the right answer stays on screen
	ReconnectGrace time.Duration // a disconnected player forfeits after this long
	FinalIdle      time.Duration // how long the result screen waits for a rematch
}

func DefaultConfig() Config {
	return Config{
		Rounds:         6,
		QPerRound:      3,
		QuestionTime:   30 * time.Second,
		PickTime:       15 * time.Second,
		PauseMax:       5 * time.Minute,
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
	phaseQuestion phase = "question" // everybody answers
	phaseReveal   phase = "reveal"   // right answer + what the others chose
	phaseFinal    phase = "final"    // standings, rematch
)

const (
	correctPoints    = 100               // a right answer; unless Config.SpeedScore, it is worth the same however fast it was given
	minCorrectPoints = correctPoints / 2 // what a right answer given at the very last moment is worth with Config.SpeedScore
	offeredCats      = 5                 // sectors of the category wheel
	tiebreakMax      = 10                // extra questions asked to break a tie before it stands as a draw
	maxSeats         = 8                 // players in a match, bots included
	extraTime        = 10 * time.Second  // what the "more time" hint adds to the question
)

// hints, by the number the browser asks for
const (
	hintFifty  = iota // two wrong options are crossed out
	hintSecond        // a wrong answer is not final: the player may answer once more
	hintExtra         // everybody gets more time for this question
	hintKinds
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
	evPause                 // seat paused (flag) or resumed (!flag) the category wheel
	evHint                  // seat uses hint n
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
	user      string  // the profile the player plays for, "" if none
	name      string
	label     string // what the others see when it differs from name (two players of the same name)
	bot       bool
	accuracy  float64 // bots: chance of a right answer
	delayLo   float64 // bots: fastest and slowest reaction, as a share of the question time
	delayHi   float64
	connected bool
	connGen   int
	left      bool
	rematch   bool
	score     int

	streak     int // right answers in a row now
	bestStreak int // ...and the longest run in this match

	used   [hintKinds]bool // hints spent in this match
	hidden []int           // options crossed out for the current question
	armed  bool            // second chance is on for the current question
}

func (s *seat) display() string {
	if s.label != "" {
		return s.label
	}
	return s.name
}

type answer struct {
	done   bool
	choice int // -1: no answer (timeout)
	points int
	ms     int // how long the player thought
}

type roundLog struct {
	cat *Category
	res [][]int // per seat, per question: 0 not played, 1 right, 2 wrong
}

type matchHooks struct {
	rematch  func(*Match)      // everybody agreed on a rematch
	ended    func(*Match)      // the match goroutine is about to exit
	answers  func([]AnswerRec) // the humans answered a question
	finished func(MatchRec)    // the match ended with a result
}

// Match is a contest between two or more seats. Its whole state is owned by
// the run goroutine; the outside world talks to it through post().
type Match struct {
	cfg         Config
	bank        *Bank
	seats       []*seat
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
	qStart   time.Time     // when the current question was shown
	qTotal   time.Duration // time the current question was given, hints included
	notice   string        // what happened to this question that everybody should know
	paused   bool          // the category wheel is frozen (phasePick only)
	pausedBy int           // seat that paused it
	timeLeft time.Duration // what was left of PickTime when it was paused
	ans      []answer
	log      []roundLog
	usedCats map[string]bool
	usedQs   map[*Question]bool
	forced   bool // too few players were left to go on: the rest is decided on the spot
	closed   bool

	tiebreak bool   // the extra question that settles a tie is being played
	tieRound int    // how many extra questions were asked so far
	tieWin   int    // seat that won the tie-break, -1 while undecided
	cand     []bool // seats that play the tie-break: the ones that share the lead
}

func newMatch(cfg Config, bank *Bank, seats []*seat, firstPicker int, hooks matchHooks) *Match {
	m := &Match{
		cfg: cfg, bank: bank, seats: seats, firstPicker: firstPicker, hooks: hooks,
		events:   make(chan event, 64),
		done:     make(chan struct{}),
		usedCats: map[string]bool{},
		usedQs:   map[*Question]bool{},
		tieWin:   -1,
		log:      make([]roundLog, cfg.Rounds),
	}
	for r := range m.log {
		m.log[r].res = make([][]int, len(seats))
		for i := range seats {
			m.log[r].res[i] = make([]int, cfg.QPerRound)
		}
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
	case evPause:
		m.onPause(ev)
	case evHint:
		m.onHint(ev)
	}
}

func (m *Match) validSeat(i int) bool { return i >= 0 && i < len(m.seats) }

func (m *Match) onTimer() {
	switch m.phase {
	case phasePick:
		if m.paused { // forgotten pause: the game goes on
			m.resume()
			return
		}
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

// nextActive is the first seat from i on (round the table) that is still in the match.
func (m *Match) nextActive(i int) int {
	for k := range m.seats {
		if j := (i + k) % len(m.seats); !m.seats[j].left {
			return j
		}
	}
	return i
}

func (m *Match) startPick() {
	m.phase = phasePick
	m.token++
	m.paused = false
	m.chooser = m.nextActive((m.firstPicker + m.round) % len(m.seats))
	m.choices = m.offerCategories(min(offeredCats, len(m.bank.Cats)))
	m.runPickTimer(m.cfg.PickTime)
}

// runPickTimer lets the chooser have `left` more time and, for a bot chooser,
// schedules its pick. It starts (or restarts, after a pause) the countdown.
func (m *Match) runPickTimer(left time.Duration) {
	m.deadline = time.Now().Add(left)
	m.broadcast()
	m.after(left, event{kind: evTimer, token: m.token})
	if m.seats[m.chooser].bot {
		m.after(botDelay(left, 0.08, 0.25), event{
			kind: evPick, seat: m.chooser, n: rand.IntN(len(m.choices)), token: m.token,
		})
	}
}

// onPause freezes or unfreezes the category wheel for everybody. Any player
// may do it, only while a category is being chosen.
func (m *Match) onPause(ev event) {
	if m.phase != phasePick || !m.validSeat(ev.seat) || ev.flag == m.paused {
		return
	}
	if !ev.flag {
		m.resume()
		return
	}
	m.paused, m.pausedBy = true, ev.seat
	m.timeLeft = max(0, time.Until(m.deadline))
	m.token++ // the pick timer and a bot's pick belong to the old token
	if m.cfg.PauseMax > 0 {
		m.after(m.cfg.PauseMax, event{kind: evTimer, token: m.token})
	}
	m.broadcast()
}

// resume gives the chooser exactly the time that was left when the pause began.
func (m *Match) resume() {
	m.paused = false
	m.token++
	m.runPickTimer(m.timeLeft)
}

func (m *Match) onPick(ev event) {
	if m.phase != phasePick || m.paused || ev.seat != m.chooser || ev.n < 0 || ev.n >= len(m.choices) {
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
	m.qs = m.drawQuestions(m.cur, m.cfg.QPerRound)
	m.qi = 0

	m.phase = phaseIntro
	m.token++
	m.deadline = time.Now().Add(m.cfg.IntroTime)
	m.broadcast()
	m.after(m.cfg.IntroTime, event{kind: evTimer, token: m.token})
}

// plays reports whether the seat answers the current question: it must still
// be in the match and, in a tie-break, share the lead.
func (m *Match) plays(i int) bool {
	if m.seats[i].left {
		return false
	}
	return !m.tiebreak || m.cand[i]
}

func (m *Match) startQuestion() {
	m.phase = phaseQuestion
	m.token++
	m.seq++
	m.ans = make([]answer, len(m.seats))
	m.qTotal = m.cfg.QuestionTime
	m.notice = ""
	m.qStart = time.Now()
	m.deadline = m.qStart.Add(m.qTotal)
	for _, s := range m.seats {
		s.hidden, s.armed = nil, false
	}
	m.broadcast()
	m.after(m.qTotal, event{kind: evTimer, token: m.token})

	for i, s := range m.seats {
		if s.bot && m.plays(i) {
			m.after(botDelay(m.cfg.QuestionTime, s.delayLo, s.delayHi), event{
				kind: evAnswer, seat: i, seq: m.seq, n: s.pickOption(m.qs[m.qi].Answer),
			})
		}
	}
}

func (m *Match) onAnswer(ev event) {
	if m.phase != phaseQuestion || ev.seq != m.seq || ev.n < 0 || ev.n >= len(m.qs[m.qi].Options) {
		return
	}
	if !m.validSeat(ev.seat) || !m.plays(ev.seat) || m.ans[ev.seat].done {
		return
	}
	s := m.seats[ev.seat]
	if slices.Contains(s.hidden, ev.n) {
		return
	}
	right := ev.n == m.qs[m.qi].Answer
	if !right && s.armed { // the second chance: this option is out, the player tries again
		s.armed = false
		s.hidden = append(s.hidden, ev.n)
		m.sendTo(ev.seat)
		return
	}
	a := answer{done: true, choice: ev.n, ms: int(time.Since(m.qStart).Milliseconds())}
	if right && !m.tiebreak {
		a.points = m.points(time.Until(m.deadline))
	}
	m.ans[ev.seat] = a
	if m.allAnswered() || (m.tiebreak && m.cfg.SpeedScore && right) { // against the clock the first right answer settles the tie-break
		m.reveal()
		return
	}
	m.broadcast() // lets the others see "answered"
}

// allAnswered reports whether nobody else can answer: humans that dropped
// off the connection are not waited for.
func (m *Match) allAnswered() bool {
	for i, s := range m.seats {
		if !m.plays(i) || m.ans[i].done {
			continue
		}
		if s.bot || s.connected {
			return false
		}
	}
	return true
}

// onHint spends one of the player's hints on the question being answered.
func (m *Match) onHint(ev event) {
	if !m.cfg.Hints || m.tiebreak || m.phase != phaseQuestion || !m.validSeat(ev.seat) || ev.n < 0 || ev.n >= hintKinds {
		return
	}
	s := m.seats[ev.seat]
	if s.left || s.bot || s.used[ev.n] || m.ans[ev.seat].done {
		return
	}
	switch ev.n {
	case hintFifty:
		var wrong []int
		for i := range m.qs[m.qi].Options {
			if i != m.qs[m.qi].Answer && !slices.Contains(s.hidden, i) {
				wrong = append(wrong, i)
			}
		}
		if len(wrong) == 0 {
			return
		}
		rand.Shuffle(len(wrong), func(i, j int) { wrong[i], wrong[j] = wrong[j], wrong[i] })
		s.hidden = append(s.hidden, wrong[:min(2, len(wrong))]...)
	case hintSecond:
		s.armed = true
	case hintExtra:
		m.qTotal += extraTime
		m.deadline = m.deadline.Add(extraTime)
		m.token++ // the old question timer is stale now
		m.after(time.Until(m.deadline), event{kind: evTimer, token: m.token})
		m.notice = s.display() + ": всем +10 секунд"
		s.used[ev.n] = true
		m.broadcast()
		return
	}
	s.used[ev.n] = true
	m.sendTo(ev.seat)
}

// points is the value of a right answer that came in with `left` of the
// question time still on the clock. With SpeedScore it falls evenly from
// correctPoints to minCorrectPoints as the time runs out.
func (m *Match) points(left time.Duration) int {
	p := correctPoints
	if m.cfg.SpeedScore {
		frac := min(1, max(0, float64(left)/float64(m.qTotal)))
		p = minCorrectPoints + int(math.Round(frac*(correctPoints-minCorrectPoints)))
	}
	if m.round == m.cfg.Rounds-1 {
		p *= 2 // the last round is worth double
	}
	return p
}

func (m *Match) reveal() {
	m.phase = phaseReveal
	m.token++
	var winners []int // tie-break: who answered right
	var recs []AnswerRec
	for i, s := range m.seats {
		if !m.ans[i].done {
			m.ans[i] = answer{done: true, choice: -1}
		}
		if !m.plays(i) {
			continue
		}
		right := m.ans[i].choice == m.qs[m.qi].Answer
		if right {
			if s.streak++; s.streak > s.bestStreak {
				s.bestStreak = s.streak
			}
		} else {
			s.streak = 0
		}
		if q := m.qs[m.qi]; !s.bot && m.ans[i].choice >= 0 { // only what people answered says how hard a question is
			recs = append(recs, AnswerRec{User: s.user, Cat: q.Cat, Key: q.Key, Text: q.Text, Right: right, Ms: m.ans[i].ms})
		}
		if m.tiebreak {
			if right {
				winners = append(winners, i)
			}
			continue
		}
		s.score += m.ans[i].points
		res := 2
		if right {
			res = 1
		}
		m.log[m.round].res[i][m.qi] = res
	}
	if m.tiebreak && len(winners) == 1 { // everybody right or everybody wrong: one more question
		m.tieWin = winners[0]
	}
	if len(recs) > 0 && m.hooks.answers != nil {
		m.hooks.answers(recs)
	}
	m.broadcast()
	m.after(m.cfg.RevealTime, event{kind: evTimer, token: m.token})
}

func (m *Match) advance() {
	switch {
	case m.tiebreak:
		alive := m.aliveCandidates()
		if len(alive) == 1 {
			m.tieWin = alive[0]
		}
		if m.tieWin >= 0 || len(alive) < 2 || m.tieRound >= tiebreakMax {
			m.finish()
			return
		}
		m.startTiebreak(alive)
	case m.qi+1 < m.cfg.QPerRound:
		m.qi++
		m.startQuestion()
	case m.round+1 < m.cfg.Rounds:
		m.round++
		m.startPick()
	case m.cfg.Tiebreak && !m.forced && len(m.leaders()) > 1:
		m.startTiebreak(m.leaders())
	default:
		m.finish()
	}
}

// leaders are the seats in the match that share the highest score.
func (m *Match) leaders() []int {
	var lead []int
	best := -1
	for i, s := range m.seats {
		switch {
		case s.left || s.score < best:
		case s.score > best:
			best, lead = s.score, []int{i}
		default:
			lead = append(lead, i)
		}
	}
	return lead
}

func (m *Match) aliveCandidates() []int {
	var alive []int
	for i := range m.seats {
		if m.cand[i] && !m.seats[i].left {
			alive = append(alive, i)
		}
	}
	return alive
}

// startTiebreak asks one more question from a random category to the players
// who share the lead. The match goes to the only one who answers it right
// (against the clock: to whoever does so first); if there is no such player,
// another question follows, up to tiebreakMax of them, and then the draw stands.
func (m *Match) startTiebreak(players []int) {
	m.tiebreak = true
	m.tieRound++
	m.cand = make([]bool, len(m.seats))
	for _, i := range players {
		m.cand[i] = true
	}
	m.cur = m.offerCategories(1)[0]
	m.qs = m.drawQuestions(m.cur, 1)
	m.qi = 0

	m.phase = phaseIntro
	m.token++
	m.deadline = time.Now().Add(m.cfg.IntroTime)
	m.broadcast()
	m.after(m.cfg.IntroTime, event{kind: evTimer, token: m.token})
}

func (m *Match) finish() {
	m.phase = phaseFinal
	m.token++
	m.paused = false
	for _, s := range m.seats {
		if s.bot {
			s.rematch = true // a bot always wants one more game
		}
	}
	if m.hooks.finished != nil {
		m.hooks.finished(m.record())
	}
	m.broadcast()
	m.after(m.cfg.FinalIdle, event{kind: evTimer, token: m.token})
}

// record is the result of the match for the statistics: the humans who have a profile.
func (m *Match) record() MatchRec {
	_, place := m.standings()
	rec := MatchRec{Players: len(m.seats), Rounds: m.cfg.Rounds, QPerRound: m.cfg.QPerRound, Speed: m.cfg.SpeedScore}
	for i, s := range m.seats {
		if s.user != "" {
			rec.Seats = append(rec.Seats, SeatRec{
				User: s.user, Place: place[i], Score: s.score, Result: m.resultFor(i), BestStrk: s.bestStreak, TieWon: i == m.tieWin,
			})
		}
	}
	return rec
}

// ---- players coming and going --------------------------------------------

func (m *Match) onConn(ev event) {
	if !m.validSeat(ev.seat) {
		return
	}
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

func (m *Match) activeSeats() (humans, all int) {
	for _, s := range m.seats {
		if s.left {
			continue
		}
		all++
		if !s.bot {
			humans++
		}
	}
	return humans, all
}

func (m *Match) onLeave(i int) {
	s := m.seats[i]
	if s.left {
		return
	}
	s.left = true
	humans, all := m.activeSeats()
	switch {
	case humans == 0:
		m.closed = true // nobody is left to play for
	case m.phase == phaseFinal:
		m.broadcast() // the others see that the rematch is smaller now
		m.checkRematch()
	case all < 2:
		m.forced = true // the one who stayed wins
		m.finish()
	case m.phase == phasePick && m.chooser == i:
		m.startPick() // the next player chooses
	case m.phase == phaseQuestion && m.allAnswered():
		m.reveal()
	default:
		m.broadcast()
	}
}

func (m *Match) onRematch(i int) {
	s := m.seats[i]
	if m.phase != phaseFinal || s.left {
		return
	}
	s.rematch = true
	m.broadcast()
	m.checkRematch()
}

// checkRematch starts the next game once every human that is still here wants it.
func (m *Match) checkRematch() {
	_, all := m.activeSeats()
	if m.phase != phaseFinal || all < 2 {
		return
	}
	for _, s := range m.seats {
		if !s.left && !s.rematch {
			return
		}
	}
	if m.hooks.rematch != nil {
		m.hooks.rematch(m)
	}
	m.closed = true
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
	need := min(n, 2) // a wheel with a single sector is no choice at all
	pool := m.eligible()
	if len(pool) < need {
		clear(m.usedCats)
		pool = m.eligible()
	}
	if len(pool) < need {
		clear(m.usedQs)
		pool = m.eligible()
	}
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
	if len(pool) > n {
		pool = pool[:n]
	}
	return pool
}

func (m *Match) drawQuestions(c *Category, n int) []Asked {
	free := m.freeQuestions(c)
	if len(free) < n {
		clear(m.usedQs)
		free = m.freeQuestions(c)
	}
	rand.Shuffle(len(free), func(i, j int) { free[i], free[j] = free[j], free[i] })
	qs := make([]Asked, 0, n)
	texts := map[string]bool{}
	// template questions share their text: never ask the same wording twice in one round
	pick := free[:0:0]
	for _, q := range free {
		if len(pick) == n {
			break
		}
		if !texts[q.Text] {
			texts[q.Text] = true
			pick = append(pick, q)
		}
	}
	for len(pick) < n && len(pick) < len(free) { // too few distinct wordings: top up with repeats
		for _, q := range free {
			if !slices.Contains(pick, q) {
				pick = append(pick, q)
				break
			}
		}
	}
	for _, q := range pick {
		m.usedQs[q] = true
		a := newAsked(q)
		a.Cat, a.Key = c.ID, questionKey(c.ID, q)
		qs = append(qs, a)
	}
	return qs
}

// ---- standings -----------------------------------------------------------

// standings lists the seats from first place down (those who left go last)
// and gives every seat its place; seats with the same score share one, except
// that the winner of a tie-break stands above the others. Players who left
// have place 0.
func (m *Match) standings() (order, place []int) {
	key := func(i int) int {
		k := m.seats[i].score * 2
		if i == m.tieWin {
			k++
		}
		return k
	}
	for i, s := range m.seats {
		if !s.left {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool { return key(order[a]) > key(order[b]) })
	place = make([]int, len(m.seats))
	for k, i := range order {
		place[i] = k + 1
		if k > 0 && key(i) == key(order[k-1]) {
			place[i] = place[order[k-1]]
		}
	}
	for i, s := range m.seats {
		if s.left {
			order = append(order, i)
		}
	}
	return order, place
}

func (m *Match) resultFor(i int) string {
	_, place := m.standings()
	if place[i] != 1 {
		return "lose"
	}
	for j := range m.seats {
		if j != i && place[j] == 1 {
			return "draw"
		}
	}
	return "win"
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
	g := &GameView{
		Phase:     string(m.phase),
		Round:     m.round + 1,
		Rounds:    m.cfg.Rounds,
		QIndex:    m.qi + 1,
		QPerRound: m.cfg.QPerRound,
		Double:    m.round == m.cfg.Rounds-1 && !m.tiebreak,
		Tiebreak:  m.tieRound > 0,
		Rules:     rulesView(m.cfg),
		Me:        i,
		Chooser:   -1,
		Correct:   -1,
	}
	asked := m.phase == phaseQuestion || m.phase == phaseReveal
	for j, s := range m.seats {
		pv := PlayerView{
			Name: s.display(), Score: s.score, Bot: s.bot, Left: s.left, Rematch: s.rematch,
			Connected: j == i || s.bot || (s.connected && !s.left),
			Answer:    -1,
		}
		if asked && m.ans[j].done {
			pv.Answered = true
			if j == i || m.phase == phaseReveal {
				pv.Answer = m.ans[j].choice
			}
		}
		if m.phase == phaseReveal {
			pv.Points = m.ans[j].points
		}
		g.Players = append(g.Players, pv)
	}
	for r := range m.log {
		rv := RoundView{Res: m.log[r].res}
		if c := m.log[r].cat; c != nil {
			cv := catView(c)
			rv.Cat = &cv
		}
		g.Board = append(g.Board, rv)
	}

	switch m.phase {
	case phasePick:
		g.Chooser = m.chooser
		g.IPick = m.chooser == i
		g.ChooserName = m.seats[m.chooser].display()
		for _, c := range m.choices {
			g.Choices = append(g.Choices, catView(c))
		}
		if m.paused {
			g.Paused = true
			g.IPaused = m.pausedBy == i
			g.PausedBy = m.seats[m.pausedBy].display()
			g.TimeLeftMs = m.timeLeft.Milliseconds()
			g.TimeTotalMs = m.cfg.PickTime.Milliseconds()
		} else {
			g.setTimer(m.deadline, m.cfg.PickTime)
		}
	case phaseIntro:
		cv := catView(m.cur)
		g.Cat = &cv
	case phaseQuestion, phaseReveal:
		cv := catView(m.cur)
		g.Cat = &cv
		q := m.qs[m.qi]
		g.Question = &QuestionView{Seq: m.seq, Text: q.Text, Options: q.Options}
		g.Watching = m.tiebreak && !m.cand[i]
		if m.phase == phaseQuestion {
			g.setTimer(m.deadline, m.qTotal)
			g.Notice = m.notice
			if m.cfg.Hints && !m.tiebreak && !m.seats[i].left {
				s := m.seats[i]
				g.Hints = &HintsView{
					Fifty: !s.used[hintFifty], Second: !s.used[hintSecond], Extra: !s.used[hintExtra],
					Armed: s.armed, Hidden: append([]int{}, s.hidden...), // [], not null: the browser looks into it
				}
			}
		} else {
			g.Correct = q.Answer
		}
	case phaseFinal:
		order, place := m.standings()
		g.Round = m.cfg.Rounds
		g.Order = order
		for j := range g.Players {
			g.Players[j].Place = place[j]
		}
		g.Result = m.resultFor(i)
		g.Forfeit = m.forced
		_, all := m.activeSeats()
		g.CanRematch = all >= 2
	}
	return g
}

func (g *GameView) setTimer(deadline time.Time, total time.Duration) {
	g.TimeLeftMs = max(0, time.Until(deadline).Milliseconds())
	g.TimeTotalMs = total.Milliseconds()
}

func catView(c *Category) CatView { return CatView{ID: c.ID, Name: c.Name, Icon: c.Icon} }
