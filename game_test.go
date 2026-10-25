package main

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"
	"time"
)

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
	t   *testing.T
	hub *Hub
	id  string
	c   *Client
	sub *sub
}

func connectPlayer(t *testing.T, h *Hub, id, name string) *player {
	t.Helper()
	c, s := h.connect(id, name)
	return &player{t: t, hub: h, id: id, c: c, sub: s}
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
			if g.MyAnswer < 0 && rand.IntN(5) > 0 { // иногда «молчим» — проверяем тайм-аут
				p.must(p.hub.Answer(p.id, g.Question.Seq, rand.IntN(4)))
			}
		case "reveal":
			revealed++
			won := g.MyAnswer == g.Correct
			double := 1
			if g.Double {
				double = 2
			}
			switch {
			case won && (g.MyPoints < basePoints*double || g.MyPoints > (basePoints+bonusPoints)*double):
				p.t.Fatalf("очки за верный ответ вне диапазона: %d (×%d)", g.MyPoints, double)
			case !won && g.MyPoints != 0:
				p.t.Fatalf("очки за неверный ответ: %d", g.MyPoints)
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
	p.must(h.Bot(p.id))

	g := p.playToEnd(h.cfg)

	if !g.Opp.Bot {
		t.Error("соперник должен быть ботом")
	}
	switch {
	case g.Me.Score > g.Opp.Score && g.Result != "win",
		g.Me.Score < g.Opp.Score && g.Result != "lose",
		g.Me.Score == g.Opp.Score && g.Result != "draw":
		t.Errorf("результат %q не соответствует счёту %d:%d", g.Result, g.Me.Score, g.Opp.Score)
	}
	// табло: у каждого раунда есть категория и сыграны все вопросы
	cats := map[string]bool{}
	sum := 0
	for i, r := range g.Board {
		if r.Cat == nil {
			t.Fatalf("раунд %d без категории", i+1)
		}
		cats[r.Cat.ID] = true
		for _, v := range append(append([]int{}, r.Me...), r.Opp...) {
			if v == 0 {
				t.Fatalf("раунд %d: остались несыгранные вопросы", i+1)
			}
		}
		sum += len(r.Me)
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
	if g2.Round != 1 || g2.Me.Score != 0 || g2.Opp.Score != 0 {
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
	if ga.Opp.Name != "Борис" || gb.Opp.Name != "Анна" {
		t.Errorf("игроки перепутаны: %q / %q", ga.Opp.Name, gb.Opp.Name)
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
	if x.Me.Score != y.Opp.Score || x.Opp.Score != y.Me.Score {
		t.Errorf("игроки видят разный счёт: %d:%d и %d:%d", x.Me.Score, x.Opp.Score, y.Me.Score, y.Opp.Score)
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

	a.must(h.CreateRoom(a.id))
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
	a.nextPhase("pick")
	b.nextPhase("pick")
	if err := h.JoinRoom(b.id, st.Room); err == nil {
		t.Error("комната после старта игры должна закрываться")
	}
}

func TestCancelRoomAndQueue(t *testing.T) {
	h := testHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()

	a.must(h.CreateRoom(a.id))
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
	if g.Result != "win" || !g.Forfeit || !g.OppLeft {
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
	a.must(h.Bot(a.id))
	a.nextPhase("pick")

	h.disconnect(a.c, a.sub)
	a2 := connectPlayer(t, h, a.id, "Анна")
	g := a2.nextPhase("pick") // новое соединение сразу получает актуальное состояние матча
	if g.Round != 1 || g.Opp.Name == "" {
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
	a.must(h.Bot(a.id))

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
	for got == nil || got.MyAnswer < 0 {
		if st := a.next(); st.Game != nil && st.Game.Phase == "question" {
			got = st.Game
		}
	}
	if got.MyAnswer != 2 {
		t.Errorf("засчитан ответ %d вместо 2", got.MyAnswer)
	}
	a.must(h.Answer(a.id, q.Question.Seq, 3)) // второй ответ на тот же вопрос
	time.Sleep(50 * time.Millisecond)
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
