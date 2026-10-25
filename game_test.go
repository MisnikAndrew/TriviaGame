package main

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

func (g *GameView) me() PlayerView { return g.Players[g.Me] }

// opp is the other player of a match for two.
func (g *GameView) opp() PlayerView { return g.Players[1-g.Me] }

func fastConfig() Config {
	return Config{
		Rounds:         3,
		QPerRound:      2,
		QuestionTime:   400 * time.Millisecond,
		PickTime:       300 * time.Millisecond,
		IntroTime:      20 * time.Millisecond,
		RevealTime:     30 * time.Millisecond,
		ReconnectGrace: 150 * time.Millisecond,
		FinalIdle:      600 * time.Millisecond,
	}
}

func testHub(t *testing.T) *Hub {
	t.Helper()
	cfg := fastConfig()
	bank, err := LoadBank(embeddedData, "data", cfg.QPerRound)
	if err != nil {
		t.Fatal(err)
	}
	return NewHub(cfg, bank)
}

// player is a fake browser: it owns an SSE subscription and drives the hub
// the same way the HTTP handlers do.
type player struct {
	t    *testing.T
	hub  *Hub
	id   string
	name string
	c    *Client
	sub  *sub
}

func connectPlayer(t *testing.T, h *Hub, id, name string) *player {
	t.Helper()
	c, s := h.connect(id, name)
	return &player{t: t, hub: h, id: id, name: name, c: c, sub: s}
}

// next returns the next state pushed to this player.
func (p *player) next() ClientState {
	p.t.Helper()
	select {
	case b := <-p.sub.ch:
		var st ClientState
		if err := json.Unmarshal(b, &st); err != nil {
			p.t.Fatalf("битый JSON состояния: %v\n%s", err, b)
		}
		return st
	case <-time.After(3 * time.Second):
		p.t.Fatalf("%s: состояние не пришло за 3 секунды", p.id)
		return ClientState{}
	}
}

func (p *player) nextScreen(screen string) ClientState {
	p.t.Helper()
	for {
		if st := p.next(); st.Screen == screen {
			return st
		}
	}
}

// quiet reports how many states were pushed to this player during d.
func (p *player) quiet(d time.Duration) int {
	p.t.Helper()
	n := 0
	for end := time.After(d); ; {
		select {
		case <-p.sub.ch:
			n++
		case <-end:
			return n
		}
	}
}

func (p *player) nextPhase(phase string) *GameView {
	p.t.Helper()
	for {
		if st := p.next(); st.Game != nil && st.Game.Phase == phase {
			return st.Game
		}
	}
}

// playToEnd answers randomly and picks categories until the final screen,
// checking the invariants of every question on the way.
func (p *player) playToEnd(cfg Config) *GameView {
	p.t.Helper()
	revealed := 0
	for {
		st := p.next()
		g := st.Game
		if g == nil {
			continue
		}
		switch g.Phase {
		case "pick":
			if g.IPick {
				p.must(p.hub.Pick(p.id, rand.IntN(len(g.Choices))))
			}
		case "question":
			if g.Correct != -1 {
				p.t.Fatal("правильный ответ утёк до раскрытия")
			}
			if g.me().Answer < 0 && rand.IntN(5) > 0 { // иногда «молчим» — проверяем тайм-аут
				p.must(p.hub.Answer(p.id, g.Question.Seq, rand.IntN(4)))
			}
		case "reveal":
			if g.Tiebreak {
				continue // очки не начисляются, их проверяют отдельные тесты
			}
			revealed++
			double := 1
			if g.Double {
				double = 2
			}
			// игрок отвечает сразу, бот — позже, но за верный ответ оба получают поровну
			for _, who := range []struct {
				name           string
				choice, points int
			}{{"я", g.me().Answer, g.me().Points}, {"соперник", g.opp().Answer, g.opp().Points}} {
				switch won := who.choice == g.Correct; {
				case won && who.points != correctPoints*double:
					p.t.Fatalf("%s: за верный ответ %d очков вместо %d (быстрота не должна влиять)", who.name, who.points, correctPoints*double)
				case !won && who.points != 0:
					p.t.Fatalf("%s: очки за неверный ответ: %d", who.name, who.points)
				}
			}
		case "final":
			if want := cfg.Rounds * cfg.QPerRound; revealed != want {
				p.t.Fatalf("раскрыто вопросов %d, ожидалось %d", revealed, want)
			}
			return g
		}
	}
}

func (p *player) must(err error) {
	p.t.Helper()
	if err != nil {
		p.t.Fatal(err)
	}
}

func TestBotMatchRunsToTheEnd(t *testing.T) {
	h := testHub(t)
	p := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	if st := p.next(); st.Screen != "menu" || st.Name != "Анна" {
		t.Fatalf("после подключения ожидалось меню, получили %+v", st)
	}
	p.must(h.Bot(p.id, GameOpts{}))

	g := p.playToEnd(h.cfg)

	if !g.opp().Bot {
		t.Error("соперник должен быть ботом")
	}
	switch {
	case g.me().Score > g.opp().Score && g.Result != "win",
		g.me().Score < g.opp().Score && g.Result != "lose",
		g.me().Score == g.opp().Score && g.Result != "draw":
		t.Errorf("результат %q не соответствует счёту %d:%d", g.Result, g.me().Score, g.opp().Score)
	}
	// табло: у каждого раунда есть категория и сыграны все вопросы
	cats := map[string]bool{}
	sum := 0
	for i, r := range g.Board {
		if r.Cat == nil {
			t.Fatalf("раунд %d без категории", i+1)
		}
		cats[r.Cat.ID] = true
		for _, v := range slices.Concat(r.Res...) {
			if v == 0 {
				t.Fatalf("раунд %d: остались несыгранные вопросы", i+1)
			}
		}
		sum += len(r.Res[0])
	}
	if len(cats) != h.cfg.Rounds {
		t.Errorf("категории в матче повторились: %v", cats)
	}
	if sum != h.cfg.Rounds*h.cfg.QPerRound {
		t.Errorf("на табло %d вопросов", sum)
	}

	// реванш с ботом принимается сразу и обнуляет счёт
	p.must(h.Rematch(p.id))
	g2 := p.nextPhase("pick")
	if g2.Round != 1 || g2.me().Score != 0 || g2.opp().Score != 0 {
		t.Errorf("реванш должен начинаться с нуля: %+v", g2)
	}
	p.must(h.Leave(p.id))
	if st := p.nextScreen("menu"); st.Screen != "menu" {
		t.Error("после выхода должно быть меню")
	}
}

func TestQuickMatchPairsTwoHumans(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()

	a.must(h.Quick(a.id))
	if st := a.next(); st.Screen != "queue" {
		t.Fatalf("первый игрок должен ждать в очереди, получили %q", st.Screen)
	}
	if err := h.Quick(a.id); err == nil {
		t.Error("повторный вход в очередь должен отклоняться")
	}
	b.must(h.Quick(b.id))

	ga, gb := a.nextPhase("pick"), b.nextPhase("pick")
	if ga.IPick == gb.IPick {
		t.Errorf("категорию в первом раунде выбирает ровно один: a=%v b=%v", ga.IPick, gb.IPick)
	}
	if ga.opp().Name != "Борис" || gb.opp().Name != "Анна" {
		t.Errorf("игроки перепутаны: %q / %q", ga.opp().Name, gb.opp().Name)
	}
	if len(ga.Choices) != offeredCats {
		t.Errorf("предложено %d категорий", len(ga.Choices))
	}

	// играем оба до конца: счёт у обоих должен совпадать зеркально
	done := make(chan *GameView, 2)
	for _, p := range []*player{a, b} {
		go func() {
			var g *GameView
			defer func() { done <- g }() // и при провале проверки в playToEnd
			g = p.playToEnd(h.cfg)
		}()
	}
	x, y := <-done, <-done
	if x == nil || y == nil {
		t.FailNow()
	}
	if x.me().Score != y.opp().Score || x.opp().Score != y.me().Score {
		t.Errorf("игроки видят разный счёт: %d:%d и %d:%d", x.me().Score, x.opp().Score, y.me().Score, y.opp().Score)
	}
	if x.Result == "win" && y.Result == "win" {
		t.Error("оба победить не могут")
	}
}

func TestPrivateRoom(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()

	a.must(h.CreateRoom(a.id, GameOpts{}))
	st := a.next()
	if st.Screen != "room" || len(st.Room) != roomCodeLen {
		t.Fatalf("ожидалась комната с кодом, получили %+v", st)
	}
	if err := h.JoinRoom(a.id, st.Room); err == nil {
		t.Error("в свою комнату входить нельзя")
	}
	if err := h.JoinRoom(b.id, "ZZZZ"); err == nil {
		t.Error("несуществующая комната должна отклоняться")
	}
	// регистр и пробелы в коде не важны
	b.must(h.JoinRoom(b.id, " "+strings.ToLower(st.Room)+" "))
	a.must(h.Start(a.id))
	a.nextPhase("pick")
	b.nextPhase("pick")
	if err := h.JoinRoom(b.id, st.Room); err == nil {
		t.Error("комната после старта игры должна закрываться")
	}
}

// rushToEnd picks the first category and gives the right answer at once, until
// the final screen. Every answer is checked against the scoring mode of cfg:
// flat, or somewhere between minCorrectPoints and correctPoints.
func (p *player) rushToEnd(cfg Config) *GameView {
	p.t.Helper()
	right := map[string]string{}
	for _, c := range p.hub.bank.Cats {
		for _, q := range c.Questions {
			right[q.Text] = q.Correct
		}
	}
	for {
		g := p.next().Game
		if g == nil {
			continue
		}
		switch g.Phase {
		case "pick":
			if g.IPick {
				p.must(p.hub.Pick(p.id, 0))
			}
		case "question":
			if g.me().Answer < 0 {
				p.must(p.hub.Answer(p.id, g.Question.Seq, slices.Index(g.Question.Options, right[g.Question.Text])))
			}
		case "reveal":
			if g.Tiebreak {
				continue
			}
			double := 1
			if g.Double {
				double = 2
			}
			if g.me().Answer != g.Correct {
				p.t.Fatalf("ответ не совпал с верным: %d вместо %d", g.me().Answer, g.Correct)
			}
			switch pts := g.me().Points; {
			case !cfg.SpeedScore && pts != correctPoints*double:
				p.t.Fatalf("без учёта времени за верный ответ %d очков вместо %d", pts, correctPoints*double)
			case cfg.SpeedScore && (pts <= minCorrectPoints*double || pts > correctPoints*double):
				p.t.Fatalf("мгновенный верный ответ с учётом времени: %d очков, ожидалось от %d до %d", pts, minCorrectPoints*double+1, correctPoints*double)
			}
		case "final":
			return g
		}
	}
}

func TestRoomRulesReachTheMatchAndSurviveRematch(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()

	a.must(h.CreateRoom(a.id, GameOpts{Seconds: 7, Speed: true, Tiebreak: true}))
	st := a.next()
	if st.Rules == nil || *st.Rules != (RulesView{Seconds: 7, Speed: true, Tiebreak: true, Rounds: 3, QPerRound: 2}) {
		t.Fatalf("в комнате должны быть видны выбранные правила, получили %+v", st.Rules)
	}
	b.must(h.JoinRoom(b.id, st.Room))
	a.must(h.Start(a.id))
	if g := a.nextPhase("pick"); g.Rules != (RulesView{Seconds: 7, Speed: true, Tiebreak: true, Rounds: 3, QPerRound: 2}) {
		t.Errorf("правила матча: %+v", g.Rules)
	}
	if g := b.nextPhase("pick"); g.Rules != (RulesView{Seconds: 7, Speed: true, Tiebreak: true, Rounds: 3, QPerRound: 2}) {
		t.Errorf("второй игрок видит другие правила: %+v", g.Rules)
	}
	b.must(h.Leave(b.id))
	a.nextScreen("menu")

	// время и режим берутся из комнаты, а не из настроек сервера
	cfg := h.cfg
	cfg.SpeedScore = true
	cfg.QuestionTime = 3 * time.Second
	a.must(h.CreateRoom(a.id, GameOpts{Seconds: 3, Speed: true}))
	b.must(h.JoinRoom(b.id, a.nextScreen("room").Room))
	a.must(h.Start(a.id))

	done := make(chan *GameView, 2)
	for _, p := range []*player{a, b} {
		go func() {
			var g *GameView
			defer func() { done <- g }()
			g = p.rushToEnd(cfg)
		}()
	}
	if x, y := <-done, <-done; x == nil || y == nil {
		t.FailNow()
	}

	// реванш играется по тем же правилам
	a.must(h.Rematch(a.id))
	b.must(h.Rematch(b.id))
	for _, p := range []*player{a, b} {
		g := p.nextPhase("pick")
		if g.Rules != (RulesView{Seconds: 3, Speed: true, Rounds: 3, QPerRound: 2}) || g.Round != 1 {
			t.Errorf("реванш: %+v", g)
		}
	}
}

func TestRoomRejectsAbsurdTimes(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()

	for _, secs := range []int{-5, 1, minQuestionSecs - 1, maxQuestionSecs + 1, 100000} {
		if err := h.CreateRoom(a.id, GameOpts{Seconds: secs}); err == nil {
			t.Errorf("%d секунд на ответ должны отклоняться", secs)
		}
	}
	// отказ ничего не ломает: комнату всё ещё можно создать
	a.must(h.CreateRoom(a.id, GameOpts{Seconds: maxQuestionSecs}))
	if st := a.nextScreen("room"); st.Rules == nil || st.Rules.Seconds != maxQuestionSecs || st.Rules.Speed {
		t.Errorf("правила комнаты: %+v", st.Rules)
	}
}

func TestPointsDependOnTimeOnlyInSpeedMode(t *testing.T) {
	newTestMatch := func(speed bool, round int) *Match {
		cfg := fastConfig()
		cfg.QuestionTime = 10 * time.Second
		cfg.SpeedScore = speed
		m := newMatch(cfg, nil, []*seat{}, 0, matchHooks{})
		m.round = round
		m.qTotal = cfg.QuestionTime
		return m
	}
	const total = 10 * time.Second
	for _, tc := range []struct {
		name  string
		speed bool
		round int
		left  time.Duration
		want  int
	}{
		{"без учёта времени: сразу", false, 0, total, 100},
		{"без учёта времени: в последний момент", false, 0, 0, 100},
		{"с учётом: сразу", true, 0, total, 100},
		{"с учётом: на середине", true, 0, total / 2, 75},
		{"с учётом: в последний момент", true, 0, 0, 50},
		{"с учётом: опоздание не уходит ниже минимума", true, 0, -time.Second, 50},
		{"с учётом: время больше лимита не даёт бонус", true, 0, 2 * total, 100},
		{"с учётом: последний раунд ×2", true, 2, total / 2, 150},
		{"без учёта: последний раунд ×2", false, 2, 0, 200},
	} {
		if got := newTestMatch(tc.speed, tc.round).points(tc.left); got != tc.want {
			t.Errorf("%s: %d очков, ожидалось %d", tc.name, got, tc.want)
		}
	}
}

func TestCancelRoomAndQueue(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()

	a.must(h.CreateRoom(a.id, GameOpts{}))
	code := a.next().Room
	a.must(h.Cancel(a.id))
	if st := a.next(); st.Screen != "menu" {
		t.Fatalf("после отмены ожидалось меню, получили %q", st.Screen)
	}
	if err := h.JoinRoom(b.id, code); err == nil {
		t.Error("отменённая комната не должна принимать игроков")
	}

	a.must(h.Quick(a.id))
	a.must(h.Cancel(a.id))
	b.must(h.Quick(b.id)) // a ушёл из очереди — b должен ждать, а не играть с призраком
	if st := b.nextScreen("queue"); st.Screen != "queue" {
		t.Error("b должен остаться в очереди")
	}
}

func TestLeaveGivesOpponentTheWin(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()
	a.must(h.Quick(a.id))
	b.must(h.Quick(b.id))
	a.nextPhase("pick")
	b.nextPhase("pick")

	a.must(h.Leave(a.id))
	g := b.nextPhase("final")
	if g.Result != "win" || !g.Forfeit || !g.opp().Left {
		t.Errorf("оставшийся игрок должен выиграть: %+v", g)
	}
	if err := h.Pick(a.id, 0); err == nil {
		t.Error("вышедший игрок больше не в матче")
	}
	// реванш невозможен, когда соперник ушёл; выход в меню завершает матч
	b.must(h.Rematch(b.id))
	b.must(h.Leave(b.id))
	b.nextScreen("menu")
}

func TestDisconnectForfeitsAfterGrace(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()
	a.must(h.Quick(a.id))
	b.must(h.Quick(b.id))
	a.nextPhase("pick")
	b.nextPhase("pick")

	h.disconnect(a.c, a.sub)
	if g := b.nextPhase("final"); g.Result != "win" || !g.Forfeit {
		t.Errorf("после долгого отключения соперника победа засчитывается: %+v", g)
	}
}

func TestReconnectWithinGraceKeepsGame(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	a.must(h.Bot(a.id, GameOpts{}))
	a.nextPhase("pick")

	h.disconnect(a.c, a.sub)
	a2 := connectPlayer(t, h, a.id, "Анна")
	g := a2.nextPhase("pick") // новое соединение сразу получает актуальное состояние матча
	if g.Round != 1 || g.opp().Name == "" {
		t.Errorf("после переподключения игра должна продолжаться: %+v", g)
	}
	time.Sleep(2 * h.cfg.ReconnectGrace) // grace истёк, но игрок вернулся — не вылетел
	if err := h.Pick(a.id, 0); err != nil {
		t.Errorf("игрок должен остаться в матче: %v", err)
	}
}

func TestNewConnectionReplacesOld(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	a2 := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	select {
	case <-a.sub.quit:
	default:
		t.Error("старое соединение должно получить сигнал замены")
	}
	h.disconnect(a.c, a.sub) // закрытие старого потока не должно считаться отключением игрока
	if !a2.c.connected() {
		t.Error("новое соединение сброшено закрытием старого")
	}
}

func TestStaleAndInvalidAnswersAreIgnored(t *testing.T) {
	h := testHub(t)
	h.cfg.QuestionTime = 2 * time.Second // времени достаточно, чтобы проверить руками
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	a.must(h.Bot(a.id, GameOpts{}))

	g := a.nextPhase("pick")
	if g.IPick {
		a.must(h.Pick(a.id, 0))
	}
	q := a.nextPhase("question")
	a.must(h.Answer(a.id, q.Question.Seq+7, 1)) // вопрос с другим номером
	a.must(h.Answer(a.id, q.Question.Seq, 9))   // варианта 9 не существует
	time.Sleep(50 * time.Millisecond)
	a.must(h.Answer(a.id, q.Question.Seq, 2))
	var got *GameView
	for got == nil || got.me().Answer < 0 {
		if st := a.next(); st.Game != nil && st.Game.Phase == "question" {
			got = st.Game
		}
	}
	if got.me().Answer != 2 {
		t.Errorf("засчитан ответ %d вместо 2", got.me().Answer)
	}
	a.must(h.Answer(a.id, q.Question.Seq, 3)) // второй ответ на тот же вопрос
	time.Sleep(50 * time.Millisecond)
	a.must(h.Leave(a.id))
}

// twoInPick starts a match between two humans and returns them as (chooser, other).
func twoInPick(t *testing.T, h *Hub) (chooser, other *player) {
	t.Helper()
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()
	a.must(h.Quick(a.id))
	b.must(h.Quick(b.id))
	ga := a.nextPhase("pick")
	b.nextPhase("pick")
	if ga.IPick {
		return a, b
	}
	return b, a
}

func TestPauseFreezesTheWheel(t *testing.T) {
	h := testHub(t)
	chooser, other := twoInPick(t, h)

	// паузу ставит не тот, кто выбирает: это право обоих игроков
	other.must(h.Pause(other.id, true))
	gc := chooser.nextPhase("pick")
	if !gc.Paused || gc.IPaused || gc.PausedBy != other.name {
		t.Errorf("выбирающий должен видеть паузу соперника: %+v", gc)
	}
	if go2 := other.nextPhase("pick"); !go2.Paused || !go2.IPaused {
		t.Errorf("поставивший паузу должен видеть, что пауза его: %+v", go2)
	}
	if gc.TimeLeftMs <= 0 || gc.TimeLeftMs > gc.TimeTotalMs {
		t.Errorf("на паузе должно оставаться время выбора: %d из %d мс", gc.TimeLeftMs, gc.TimeTotalMs)
	}

	// время выбора не идёт, а выбрать категорию на паузе нельзя
	chooser.must(h.Pick(chooser.id, 0))
	time.Sleep(2 * h.cfg.PickTime)
	if n, m := len(chooser.sub.ch), len(other.sub.ch); n+m != 0 {
		t.Fatalf("пока игра на паузе, ничего не должно происходить (пришло %d+%d состояний)", n, m)
	}

	// снять паузу может любой из двоих — и время продолжается с того же места
	chooser.must(h.Pause(chooser.id, false))
	gr := other.nextPhase("pick")
	if gr.Paused || gr.TimeLeftMs > gc.TimeLeftMs || gr.TimeLeftMs < gc.TimeLeftMs-100 {
		t.Errorf("после паузы осталось %d мс, до неё было %d: %+v", gr.TimeLeftMs, gc.TimeLeftMs, gr)
	}
	other.nextPhase("intro") // время вышло — категория выбрана случайно
}

func TestPauseEndsByItself(t *testing.T) {
	h := testHub(t)
	h.cfg.PauseMax = 150 * time.Millisecond
	chooser, other := twoInPick(t, h)

	chooser.must(h.Pause(chooser.id, true))
	if g := other.nextPhase("pick"); !g.Paused {
		t.Fatalf("игра должна встать на паузу: %+v", g)
	}
	start := time.Now()
	if g := other.nextPhase("pick"); g.Paused {
		t.Errorf("забытая пауза должна сняться сама: %+v", g)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Errorf("пауза закончилась слишком рано: через %v", d)
	}
	other.nextPhase("intro")
}

func TestPauseWhileBotChooses(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	h.mu.Lock()
	h.launch([]*seat{h.humanSeat(a.c), newBotSeat(botNormal)}, 1, h.defaultRules()) // в первом раунде категорию выбирает бот
	h.mu.Unlock()
	if g := a.nextPhase("pick"); g.IPick {
		t.Fatalf("категорию должен выбирать бот: %+v", g)
	}

	a.must(h.Pause(a.id, true))
	if g := a.nextPhase("pick"); !g.Paused {
		t.Fatalf("игра должна встать на паузу: %+v", g)
	}
	if n := a.quiet(2 * h.cfg.PickTime); n != 0 {
		t.Fatalf("бот не должен выбирать категорию на паузе (пришло %d состояний)", n)
	}
	a.must(h.Pause(a.id, false))
	a.nextPhase("intro") // бот (или таймер) продолжает с того места, где остановились
}

func TestPauseOutsideTheWheelIsIgnored(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	a.must(h.Bot(a.id, GameOpts{}))
	if g := a.nextPhase("pick"); g.IPick {
		a.must(h.Pick(a.id, 0))
	}
	a.nextPhase("question")
	a.must(h.Pause(a.id, true)) // во время вопроса пауза не предусмотрена
	a.nextPhase("reveal")       // время вопроса идёт как обычно

	// пауза не должна «просочиться» в следующее колесо
	if g := a.nextPhase("pick"); g.Paused {
		t.Fatalf("лишняя пауза попала в следующий выбор категории: %+v", g)
	}
	a.must(h.Leave(a.id))
}

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"":                  "Игрок",
		"   ":               "Игрок",
		"  Анна   Иванова ": "Анна Иванова",
		"a\x00b\nc":         "abc",
		"ОченьДлинноеИмяКоторое":    "ОченьДлинноеИмяК",
		"<script>alert(1)</script>": "<script>alert(1)",
	}
	for in, want := range cases {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

// rightOption is the index of the right answer to the question on screen.
func (p *player) rightOption(g *GameView) int {
	p.t.Helper()
	for _, c := range p.hub.bank.Cats {
		for _, q := range c.Questions {
			if q.Text == g.Question.Text {
				return slices.Index(g.Question.Options, q.Correct)
			}
		}
	}
	p.t.Fatalf("вопрос %q не найден в базе", g.Question.Text)
	return -1
}

// playTied answers every regular question right, so that two such players end
// level, and answers the tie-break with tie(rightIndex): an option, or -1 to
// stay silent. It returns the final screen and how many tie-break questions
// were revealed on the way.
func (p *player) playTied(tie func(right int) int) (*GameView, int) {
	p.t.Helper()
	ties := 0
	for {
		g := p.next().Game
		if g == nil {
			continue
		}
		switch g.Phase {
		case "pick":
			if g.IPick {
				p.must(p.hub.Pick(p.id, 0))
			}
		case "question":
			if g.me().Answer >= 0 {
				continue
			}
			choice := p.rightOption(g)
			if g.Tiebreak {
				choice = tie(choice)
			}
			if choice >= 0 {
				p.must(p.hub.Answer(p.id, g.Question.Seq, choice))
			}
		case "reveal":
			if g.Tiebreak {
				ties++
			}
		case "final":
			return g, ties
		}
	}
}

func startTiedRoom(t *testing.T, h *Hub) (a, b *player) {
	t.Helper()
	a = connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b = connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()
	a.must(h.CreateRoom(a.id, GameOpts{Rounds: 1, QPerRound: 1, Tiebreak: true}))
	b.must(h.JoinRoom(b.id, a.nextScreen("room").Room))
	a.must(h.Start(a.id))
	return a, b
}

func TestTiebreakGoesToTheFirstRightAnswer(t *testing.T) {
	h := testHub(t)
	a, b := startTiedRoom(t, h)

	type res struct {
		g    *GameView
		ties int
	}
	out := make(chan res, 2)
	go func() { g, n := a.playTied(func(right int) int { return right }); out <- res{g, n} }()
	go func() { g, n := b.playTied(func(int) int { return -1 }); out <- res{g, n} }() // молчит
	x, y := <-out, <-out
	if x.g == nil || y.g == nil {
		t.FailNow()
	}
	for _, r := range []res{x, y} {
		if !r.g.Tiebreak || r.ties != 1 || r.g.me().Score != r.g.opp().Score {
			t.Errorf("счёт %d:%d, решающих вопросов %d, tiebreak=%v", r.g.me().Score, r.g.opp().Score, r.ties, r.g.Tiebreak)
		}
	}
	if got := map[string]bool{x.g.Result: true, y.g.Result: true}; !got["win"] || !got["lose"] {
		t.Errorf("результаты %q и %q: победитель должен определиться", x.g.Result, y.g.Result)
	}
}

func TestTiebreakEndsInDrawAfterTheLimit(t *testing.T) {
	h := testHub(t)
	a, b := startTiedRoom(t, h)

	wrong := func(right int) int { return (right + 1) % 4 }
	type res struct {
		g    *GameView
		ties int
	}
	out := make(chan res, 2)
	for _, p := range []*player{a, b} {
		go func() { g, n := p.playTied(wrong); out <- res{g, n} }()
	}
	for range 2 {
		r := <-out
		if r.g == nil {
			t.FailNow()
		}
		if r.g.Result != "draw" || r.ties != tiebreakMax {
			t.Errorf("результат %q после %d решающих вопросов, ожидалась ничья после %d", r.g.Result, r.ties, tiebreakMax)
		}
	}
}

func TestNoTiebreakWhenSomeoneLeads(t *testing.T) {
	h := testHub(t)
	a, b := startTiedRoom(t, h)
	// Борис всегда отвечает неверно: ничьей не будет
	out := make(chan [2]int, 2)
	go func() { _, n := a.playTied(func(r int) int { return r }); out <- [2]int{0, n} }()
	go func() {
		for {
			g := b.next().Game
			switch {
			case g == nil:
			case g.Phase == "pick" && g.IPick:
				b.must(h.Pick(b.id, 0))
			case g.Phase == "question" && g.me().Answer < 0:
				b.must(h.Answer(b.id, g.Question.Seq, (b.rightOption(g)+1)%4))
			case g.Phase == "final":
				if g.Tiebreak {
					t.Error("решающий вопрос при разном счёте")
				}
				out <- [2]int{1, 0}
				return
			}
		}
	}()
	<-out
	<-out
}

func TestMatchSizeAndCategoriesFromRoom(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	only := []string{h.bank.Cats[0].ID, h.bank.Cats[1].ID}

	// раундов больше, чем тем: темы повторяются, но играются только выбранные
	cfg := h.cfg
	cfg.Rounds, cfg.QPerRound = 4, 1
	a.must(h.Bot(a.id, GameOpts{Rounds: 4, QPerRound: 1, Cats: only, Level: "hard"}))
	g := a.nextPhase("pick")
	if g.Rules.Rounds != 4 || g.Rules.QPerRound != 1 || len(g.Board) != 4 || len(g.Choices) != 2 {
		t.Fatalf("правила и колесо: %+v, на колесе %d тем", g.Rules, len(g.Choices))
	}
	g = a.playToEnd(cfg)
	for _, r := range g.Board {
		if r.Cat == nil || (r.Cat.ID != only[0] && r.Cat.ID != only[1]) {
			t.Fatalf("сыграна тема вне выбранных: %+v", r.Cat)
		}
	}
}

func TestRoomShowsItsCategories(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()

	a.must(h.CreateRoom(a.id, GameOpts{Cats: []string{h.bank.Cats[0].ID, h.bank.Cats[2].ID}}))
	if st := a.nextScreen("room"); len(st.RoomCats) != 2 || st.RoomCats[0].ID != h.bank.Cats[0].ID {
		t.Errorf("темы комнаты: %+v", st.RoomCats)
	}
	a.must(h.Cancel(a.id))
	a.nextScreen("menu")
	a.must(h.CreateRoom(a.id, GameOpts{}))
	if st := a.nextScreen("room"); len(st.RoomCats) != 0 {
		t.Errorf("в комнате со всеми темами список не нужен: %+v", st.RoomCats)
	}
}

func TestGameOptsAreValidated(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	one, two := h.bank.Cats[0].ID, h.bank.Cats[1].ID
	for name, o := range map[string]GameOpts{
		"слишком много раундов":       {Rounds: maxRounds + 1},
		"слишком много вопросов":      {QPerRound: maxQPerRound + 1},
		"отрицательные раунды":        {Rounds: -1},
		"одна тема":                   {Cats: []string{one}},
		"неизвестная тема":            {Cats: []string{one, "нет-такой"}},
		"неизвестная сложность":       {Level: "impossible"},
		"вопросов больше, чем в теме": {QPerRound: maxQPerRound, Cats: []string{one, two}, Rounds: 1, Seconds: 0, Speed: false, Level: "x"},
	} {
		if err := h.CreateRoom(a.id, o); err == nil {
			t.Errorf("%s: комната создана", name)
		}
		if err := h.Bot(a.id, o); err == nil {
			t.Errorf("%s: игра с ботом началась", name)
		}
	}
	a.must(h.CreateRoom(a.id, GameOpts{Rounds: maxRounds, QPerRound: maxQPerRound})) // границы допустимы
}

func TestBotLevels(t *testing.T) {
	for name, l := range map[string]botLevel{"easy": botEasy, "normal": botNormal, "hard": botHard} {
		for range 200 {
			s := newBotSeat(l)
			if s.accuracy < l.accLo || s.accuracy > l.accHi || s.delayLo != l.delayLo || s.delayHi != l.delayHi {
				t.Fatalf("%s: бот %+v вне диапазона %+v", name, s, l)
			}
		}
	}
	if !(botEasy.accHi <= botNormal.accLo && botNormal.accHi <= botHard.accLo) {
		t.Error("уровни бота должны идти по возрастанию точности")
	}
}

func TestTiebreakWithoutTimeRepeatsUntilExactlyOneIsRight(t *testing.T) {
	h := testHub(t)
	a, b := startTiedRoom(t, h)

	type res struct {
		g    *GameView
		ties int
	}
	out := make(chan res, 2)
	// первый решающий вопрос оба решают верно, на втором Борис ошибается
	go func() { g, n := a.playTied(func(right int) int { return right }); out <- res{g, n} }()
	nb := 0
	go func() {
		g, n := b.playTied(func(right int) int {
			if nb++; nb == 1 {
				return right
			}
			return (right + 1) % 4
		})
		out <- res{g, n}
	}()
	for range 2 {
		r := <-out
		if r.g == nil {
			t.FailNow()
		}
		if r.ties != 2 {
			t.Errorf("решающих вопросов %d, ожидалось 2", r.ties)
		}
		if r.g.me().Name == "Анна" && r.g.Result != "win" || r.g.me().Name == "Борис" && r.g.Result != "lose" {
			t.Errorf("%s: результат %q", r.g.me().Name, r.g.Result)
		}
	}
}

func TestTiebreakIsOptional(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()
	a.must(h.CreateRoom(a.id, GameOpts{Rounds: 1, QPerRound: 1}))
	b.must(h.JoinRoom(b.id, a.nextScreen("room").Room))
	a.must(h.Start(a.id))

	out := make(chan *GameView, 2)
	for _, p := range []*player{a, b} {
		go func() { g, _ := p.playTied(func(r int) int { return r }); out <- g }()
	}
	for range 2 {
		if g := <-out; g == nil || g.Tiebreak || g.Result != "draw" {
			t.Fatalf("без опции равный счёт — ничья без решающих вопросов: %+v", g)
		}
	}
}

// ---- hints ---------------------------------------------------------------

// toQuestion picks categories (when it is this player's turn) until a question is on screen.
func (p *player) toQuestion() *GameView {
	p.t.Helper()
	for {
		g := p.next().Game
		switch {
		case g == nil:
		case g.Phase == "pick" && g.IPick:
			p.must(p.hub.Pick(p.id, 0))
		case g.Phase == "question":
			return g
		}
	}
}

// until returns the first state of the game that satisfies cond.
func (p *player) until(cond func(*GameView) bool) *GameView {
	p.t.Helper()
	for {
		if g := p.next().Game; g != nil && cond(g) {
			return g
		}
	}
}

func hintGame(t *testing.T, hints bool) (*Hub, *player, *GameView) {
	t.Helper()
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	a.must(h.Bot(a.id, GameOpts{Seconds: 3, Hints: hints, Level: "easy"}))
	return h, a, a.toQuestion()
}

func TestNoHintsUnlessTheRoomHasThem(t *testing.T) {
	_, _, g := hintGame(t, false)
	if g.Hints != nil || g.Rules.Hints {
		t.Errorf("подсказок быть не должно: %+v", g.Hints)
	}
}

func TestHintFiftyCrossesOutTwoWrongOptions(t *testing.T) {
	h, a, g := hintGame(t, true)
	if g.Hints == nil || !g.Hints.Fifty || !g.Hints.Second || !g.Hints.Extra || g.Hints.Hidden == nil || len(g.Hints.Hidden) != 0 {
		t.Fatalf("в начале все подсказки доступны: %+v", g.Hints)
	}
	right := a.rightOption(g)
	a.must(h.Hint(a.id, "fifty"))
	g = a.until(func(g *GameView) bool { return g.Hints != nil && !g.Hints.Fifty })
	if len(g.Hints.Hidden) != 2 || slices.Contains(g.Hints.Hidden, right) {
		t.Fatalf("убраны должны быть два неверных варианта: %v (верный %d)", g.Hints.Hidden, right)
	}
	// убранный вариант выбрать нельзя, оставшийся верный — можно
	a.must(h.Answer(a.id, g.Question.Seq, g.Hints.Hidden[0]))
	a.must(h.Answer(a.id, g.Question.Seq, right))
	if r := a.until(func(g *GameView) bool { return g.Phase == "reveal" }); r.me().Answer != right {
		t.Errorf("засчитан ответ %d вместо %d", r.me().Answer, right)
	}
}

func TestHintSecondChanceForgivesOneMistake(t *testing.T) {
	h, a, g := hintGame(t, true)
	right := a.rightOption(g)
	wrong := (right + 1) % 4
	a.must(h.Hint(a.id, "second"))
	g = a.until(func(g *GameView) bool { return g.Hints != nil && g.Hints.Armed })

	a.must(h.Answer(a.id, g.Question.Seq, wrong))
	g = a.until(func(g *GameView) bool { return g.Hints != nil && slices.Contains(g.Hints.Hidden, wrong) })
	if g.me().Answered || g.Hints.Armed || g.Hints.Second {
		t.Fatalf("после первой ошибки можно отвечать снова: %+v %+v", g.me(), g.Hints)
	}
	a.must(h.Answer(a.id, g.Question.Seq, right))
	r := a.until(func(g *GameView) bool { return g.Phase == "reveal" })
	if r.me().Answer != right || r.me().Points != correctPoints {
		t.Errorf("второй ответ должен засчитаться: %+v", r.me())
	}
}

func TestSecondChanceIsSpentOnTheFirstMistake(t *testing.T) {
	h, a, g := hintGame(t, true)
	right := a.rightOption(g)
	a.must(h.Hint(a.id, "second"))
	a.until(func(g *GameView) bool { return g.Hints != nil && g.Hints.Armed })
	a.must(h.Answer(a.id, g.Question.Seq, (right+1)%4)) // прощённая ошибка
	a.must(h.Answer(a.id, g.Question.Seq, (right+2)%4)) // вторая уже окончательная
	r := a.until(func(g *GameView) bool { return g.Phase == "reveal" })
	if r.me().Answer != (right+2)%4 || r.me().Points != 0 {
		t.Errorf("вторая ошибка должна быть окончательной: %+v", r.me())
	}
}

func TestHintExtraTimeGoesToEverybody(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()
	a.must(h.CreateRoom(a.id, GameOpts{Seconds: 3, Hints: true}))
	b.must(h.JoinRoom(b.id, a.nextScreen("room").Room))
	a.must(h.Start(a.id))
	go b.toQuestion()
	ga := a.toQuestion()
	if ga.TimeTotalMs != 3000 {
		t.Fatalf("время вопроса %d мс", ga.TimeTotalMs)
	}
	a.must(h.Hint(a.id, "extra"))
	ga = a.until(func(g *GameView) bool { return g.Notice != "" })
	if ga.TimeTotalMs != 13000 || ga.TimeLeftMs < 10000 || ga.Hints.Extra {
		t.Errorf("должно добавиться 10 секунд: %+v", ga)
	}
	a.must(h.Hint(a.id, "extra")) // второй раз за матч нельзя
	if n := a.quiet(200 * time.Millisecond); n != 0 {
		t.Errorf("повторная подсказка изменила игру (%d состояний)", n)
	}
}

func TestEachHintOnlyOncePerMatch(t *testing.T) {
	h, a, g := hintGame(t, true)
	a.must(h.Hint(a.id, "fifty"))
	g = a.until(func(g *GameView) bool { return g.Hints != nil && !g.Hints.Fifty })
	a.must(h.Answer(a.id, g.Question.Seq, a.rightOption(g)))
	a.until(func(g *GameView) bool { return g.Phase == "reveal" })
	g = a.toQuestion() // следующий вопрос
	if g.Hints.Fifty || len(g.Hints.Hidden) != 0 || !g.Hints.Second || !g.Hints.Extra {
		t.Errorf("«50/50» израсходована, остальные целы, вычеркнутого уже нет: %+v", g.Hints)
	}
	if err := h.Hint(a.id, "nope"); err == nil {
		t.Error("неизвестная подсказка должна отклоняться")
	}
}

// ---- rooms for many ------------------------------------------------------

func (p *player) lobby(n int) *LobbyView {
	p.t.Helper()
	for {
		if st := p.next(); st.Screen == "room" && st.Lobby != nil && len(st.Lobby.Players) == n {
			return st.Lobby
		}
	}
}

func TestRoomLobbyAndStartByHost(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	c := connectPlayer(t, h, "player-cccccccc", "Вика")
	for _, p := range []*player{a, b, c} {
		p.next()
	}
	a.must(h.CreateRoom(a.id, GameOpts{Bots: 2}))
	st := a.nextScreen("room")
	code := st.Room
	if lv := st.Lobby; !lv.Host || lv.Max != maxSeats || !lv.CanStart || !lv.Players[0].Owner || !lv.Players[1].Bot {
		t.Errorf("лобби создателя: %+v", lv)
	}
	b.must(h.JoinRoom(b.id, code))
	c.must(h.JoinRoom(c.id, code))
	if lv := b.lobby(5); lv.Host || lv.CanStart || lv.Players[1].Name != "Борис" || !lv.Players[1].Me {
		t.Errorf("лобби гостя: %+v", lv)
	}
	if err := h.Start(b.id); err == nil {
		t.Error("гость не должен запускать игру")
	}
	a.must(h.Start(a.id))

	done := make(chan *GameView, 3)
	for _, p := range []*player{a, b, c} {
		go func() {
			var g *GameView
			defer func() { done <- g }()
			g = p.rushToEnd(h.cfg)
		}()
	}
	for range 3 {
		g := <-done
		if g == nil {
			t.FailNow()
		}
		if len(g.Players) != 5 || len(g.Order) != 5 || g.Players[g.Order[0]].Place != 1 {
			t.Fatalf("итоговая таблица: %+v", g)
		}
		bots := 0
		for _, pl := range g.Players {
			if pl.Bot {
				bots++
			}
		}
		if bots != 2 {
			t.Errorf("ботов в игре %d, ожидалось 2", bots)
		}
		for k := 1; k < len(g.Order); k++ {
			if g.Players[g.Order[k-1]].Score < g.Players[g.Order[k]].Score {
				t.Errorf("таблица не по убыванию очков: %+v", g.Players)
			}
		}
	}
	if err := h.JoinRoom(c.id, code); err == nil {
		t.Error("комната закрывается, когда игра началась")
	}
}

func TestRoomNeedsTwoAndHasEightSeats(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	c := connectPlayer(t, h, "player-cccccccc", "Вика")
	for _, p := range []*player{a, b, c} {
		p.next()
	}
	a.must(h.CreateRoom(a.id, GameOpts{}))
	if err := h.Start(a.id); err == nil {
		t.Error("одному без ботов начинать нельзя")
	}
	a.must(h.Cancel(a.id))
	a.nextScreen("menu")

	a.must(h.CreateRoom(a.id, GameOpts{Bots: 6})) // создатель + 6 ботов: одно место свободно
	code := a.nextScreen("room").Room
	b.must(h.JoinRoom(b.id, code))
	if err := h.JoinRoom(c.id, code); err == nil {
		t.Error("девятого за столом быть не должно")
	}
	a.must(h.Cancel(a.id))
	a.nextScreen("menu")
	for _, bots := range []int{-1, maxSeats} {
		if err := h.CreateRoom(a.id, GameOpts{Bots: bots}); err == nil {
			t.Errorf("%d ботов: комната создана", bots)
		}
	}
	a.must(h.CreateRoom(a.id, GameOpts{Bots: maxSeats - 1}))
	if err := h.JoinRoom(b.id, a.nextScreen("room").Room); err == nil {
		t.Error("семь ботов и создатель занимают все места")
	}
}

func TestGuestLeavesAndHostClosesTheRoom(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()
	a.must(h.CreateRoom(a.id, GameOpts{}))
	code := a.nextScreen("room").Room
	b.must(h.JoinRoom(b.id, code))
	a.lobby(2)

	b.must(h.Cancel(b.id))
	if st := b.nextScreen("menu"); st.Screen != "menu" {
		t.Fatal("гость должен вернуться в меню")
	}
	a.lobby(1) // создатель видит, что остался один

	b.must(h.JoinRoom(b.id, code))
	a.must(h.Cancel(a.id))
	b.nextScreen("menu") // комната закрыта: гость снова в меню
	if err := h.JoinRoom(b.id, code); err == nil {
		t.Error("закрытая комната не должна принимать игроков")
	}
}

func TestMatchGoesOnWhenOneOfThreeLeaves(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	c := connectPlayer(t, h, "player-cccccccc", "Вика")
	for _, p := range []*player{a, b, c} {
		p.next()
	}
	a.must(h.CreateRoom(a.id, GameOpts{}))
	code := a.nextScreen("room").Room
	b.must(h.JoinRoom(b.id, code))
	c.must(h.JoinRoom(c.id, code))
	a.must(h.Start(a.id))
	for _, p := range []*player{a, b, c} {
		p.nextPhase("pick")
	}
	c.must(h.Leave(c.id))
	c.nextScreen("menu")

	done := make(chan *GameView, 2)
	for _, p := range []*player{a, b} {
		go func() {
			var g *GameView
			defer func() { done <- g }()
			g = p.rushToEnd(h.cfg)
		}()
	}
	var g *GameView
	for range 2 {
		if g = <-done; g == nil {
			t.FailNow()
		}
		if g.Forfeit || !g.Players[2].Left || g.Players[2].Place != 0 || g.Order[2] != 2 {
			t.Fatalf("игра на троих продолжается без вышедшего: %+v", g)
		}
	}

	// реванш играют оставшиеся двое
	a.must(h.Rematch(a.id))
	b.must(h.Rematch(b.id))
	for _, p := range []*player{a, b} {
		if g := p.nextPhase("pick"); len(g.Players) != 2 || g.Round != 1 {
			t.Errorf("реванш вдвоём: %+v", g.Players)
		}
	}
}

func TestLastPlayerStandingWins(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()
	a.must(h.CreateRoom(a.id, GameOpts{}))
	b.must(h.JoinRoom(b.id, a.nextScreen("room").Room))
	a.must(h.Start(a.id))
	b.nextPhase("pick")
	a.must(h.Leave(a.id))
	if g := b.nextPhase("final"); g.Result != "win" || !g.Forfeit || g.CanRematch {
		t.Errorf("оставшийся побеждает, реванша нет: %+v", g)
	}
}

func TestSameNamesAreToldApart(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Игрок")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Игрок")
	a.next()
	b.next()
	a.must(h.CreateRoom(a.id, GameOpts{}))
	b.must(h.JoinRoom(b.id, a.nextScreen("room").Room))
	a.must(h.Start(a.id))
	g := a.nextPhase("pick")
	if g.Players[0].Name == g.Players[1].Name {
		t.Errorf("одинаковые имена: %q и %q", g.Players[0].Name, g.Players[1].Name)
	}
	if st := a.next(); st.Name != "Игрок" {
		t.Errorf("собственное имя игрока не должно меняться: %q", st.Name)
	}
}

// ---- standings -----------------------------------------------------------

func TestStandingsShareFirstPlaceAndTiebreakWinnerLeads(t *testing.T) {
	m := newMatch(fastConfig(), nil, []*seat{{score: 100}, {score: 200}, {score: 200}, {score: 50}, {score: 900, left: true}}, 0, matchHooks{})
	order, place := m.standings()
	if want := []int{1, 2, 0, 3, 4}; !slices.Equal(order, want) {
		t.Errorf("порядок %v, ожидался %v", order, want)
	}
	if want := []int{3, 1, 1, 4, 0}; !slices.Equal(place, want) {
		t.Errorf("места %v, ожидались %v", place, want)
	}
	if m.resultFor(1) != "draw" || m.resultFor(0) != "lose" || m.resultFor(4) != "lose" {
		t.Errorf("результаты: %s %s %s", m.resultFor(1), m.resultFor(0), m.resultFor(4))
	}
	if got := m.leaders(); !slices.Equal(got, []int{1, 2}) {
		t.Errorf("лидеры %v (вышедший с большим счётом не считается)", got)
	}

	m.tieWin = 2
	order, place = m.standings()
	if want := []int{2, 1, 0, 3, 4}; !slices.Equal(order, want) {
		t.Errorf("с победителем решающего вопроса: порядок %v, ожидался %v", order, want)
	}
	if want := []int{3, 2, 1, 4, 0}; !slices.Equal(place, want) {
		t.Errorf("с победителем решающего вопроса: места %v, ожидались %v", place, want)
	}
	if m.resultFor(2) != "win" || m.resultFor(1) != "lose" {
		t.Errorf("победитель решающего вопроса: %s, второй: %s", m.resultFor(2), m.resultFor(1))
	}
}

func TestTiebreakIsPlayedByTheLeadersOnly(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	c := connectPlayer(t, h, "player-cccccccc", "Вика")
	for _, p := range []*player{a, b, c} {
		p.next()
	}
	a.must(h.CreateRoom(a.id, GameOpts{Rounds: 1, QPerRound: 1, Tiebreak: true}))
	code := a.nextScreen("room").Room
	b.must(h.JoinRoom(b.id, code))
	c.must(h.JoinRoom(c.id, code))
	a.must(h.Start(a.id))

	// Анна и Борис отвечают верно, Вика ошибается: лидеров двое
	type res struct {
		g *GameView
		w bool
	}
	out := make(chan res, 3)
	play := func(p *player, rightOnRound bool) {
		var fin *GameView
		watched := false
		defer func() { out <- res{fin, watched} }()
		for {
			g := p.next().Game
			switch {
			case g == nil:
			case g.Phase == "pick" && g.IPick:
				p.must(h.Pick(p.id, 0))
			case g.Phase == "question" && g.me().Answer < 0 && !g.Watching:
				choice := p.rightOption(g)
				if !g.Tiebreak && !rightOnRound {
					choice = (choice + 1) % 4
				}
				if g.Tiebreak && p == b {
					choice = (choice + 1) % 4 // Борис ошибается в решающем вопросе
				}
				p.must(h.Answer(p.id, g.Question.Seq, choice))
			case g.Phase == "question" && g.Watching:
				watched = true
			case g.Phase == "final":
				fin = g
				return
			}
		}
	}
	go play(a, true)
	go play(b, true)
	go play(c, false)
	got := map[bool]int{}
	for range 3 {
		r := <-out
		if r.g == nil {
			t.FailNow()
		}
		got[r.w]++
		if !r.g.Tiebreak {
			t.Fatalf("у лидеров равный счёт: нужен решающий вопрос: %+v", r.g.Players)
		}
	}
	if got[true] != 1 {
		t.Errorf("решающий вопрос должен был только наблюдать один игрок (Вика), наблюдали %d", got[true])
	}
}
