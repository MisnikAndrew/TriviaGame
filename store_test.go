package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newUser(t *testing.T, s *Store, name string) string {
	t.Helper()
	id, _, err := s.CreateUser(name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUsersLogInWithTheirSecretOnly(t *testing.T) {
	s := testStore(t)
	id, secret, err := s.CreateUser("Анна")
	if err != nil {
		t.Fatal(err)
	}
	if name, err := s.Login(id, secret); err != nil || name != "Анна" {
		t.Errorf("вход по своему секрету: %q, %v", name, err)
	}
	for _, bad := range [][2]string{{id, "wrong"}, {id, ""}, {"nobody", secret}, {"", ""}} {
		if _, err := s.Login(bad[0], bad[1]); err != errBadLogin {
			t.Errorf("вход %v: ошибка %v, ожидалась errBadLogin", bad, err)
		}
	}
	var stored []byte
	s.db.QueryRow(`SELECT secret_hash FROM users WHERE id = ?`, id).Scan(&stored)
	if len(stored) == 0 || string(stored) == secret {
		t.Error("секрет не должен храниться в открытом виде")
	}
	s.SetName(id, "Аня")
	s.Flush()
	if p, _ := s.Profile(id); p.Name != "Аня" {
		t.Errorf("имя не сменилось: %q", p.Name)
	}
}

func TestDatabaseSurvivesReopening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "game.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	id, secret, _ := s.CreateUser("Борис")
	s.RecordAnswers([]AnswerRec{{User: id, Cat: "history", Key: "k1", Text: "q", Right: true, Ms: 800}})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if name, err := s.Login(id, secret); err != nil || name != "Борис" {
		t.Fatalf("после перезапуска: %q, %v", name, err)
	}
	if a, c := s.QuestionStat("k1"); a != 1 || c != 1 {
		t.Errorf("статистика вопроса пропала: %d/%d", a, c)
	}
}

func TestAnswersAreCountedForQuestionAndPlayer(t *testing.T) {
	s := testStore(t)
	a, b := newUser(t, s, "Анна"), newUser(t, s, "Борис")
	s.RecordAnswers([]AnswerRec{
		{User: a, Cat: "history", Key: "k1", Text: "Вопрос 1", Right: true, Ms: 1500},
		{User: b, Cat: "history", Key: "k1", Text: "Вопрос 1", Right: false, Ms: 900},
		{User: "", Cat: "history", Key: "k1", Text: "Вопрос 1", Right: true, Ms: 700}, // игрок без профиля тоже отвечал
		{User: a, Cat: "space", Key: "k2", Text: "Вопрос 2", Right: true, Ms: 600},
		{User: a, Cat: "space", Key: "k3", Text: "Вопрос 3", Right: false, Ms: 4000},
	})
	s.Flush()

	if asked, right := s.QuestionStat("k1"); asked != 3 || right != 2 {
		t.Errorf("вопрос 1: отвечали %d раз, верно %d (ожидалось 3 и 2)", asked, right)
	}
	if asked, right := s.QuestionStat("nope"); asked != 0 || right != 0 {
		t.Errorf("неизвестный вопрос: %d/%d", asked, right)
	}
	p, _ := s.Profile(a)
	if p.Answered != 3 || p.Correct != 2 || p.FastestMs != 600 {
		t.Errorf("Анна: %+v", p)
	}
	want := []CatStat{{"space", 2, 1}, {"history", 1, 1}}
	if !slices.Equal(p.Cats, want) {
		t.Errorf("темы Анны: %v, ожидалось %v", p.Cats, want)
	}
	if pb, _ := s.Profile(b); pb.Answered != 1 || pb.Correct != 0 || pb.FastestMs != 0 {
		t.Errorf("Борис: %+v", pb)
	}
}

func TestEloDeltas(t *testing.T) {
	for _, tc := range []struct {
		name            string
		ratings, places []int
		want            []int
	}{
		{"равные, победа", []int{1000, 1000}, []int{1, 2}, []int{16, -16}},
		{"равные, ничья", []int{1000, 1000}, []int{1, 1}, []int{0, 0}},
		{"фаворит выиграл — почти ничего", []int{1400, 1000}, []int{1, 2}, []int{3, -3}},
		{"сенсация", []int{1000, 1400}, []int{1, 2}, []int{29, -29}},
		{"трое равных", []int{1000, 1000, 1000}, []int{1, 2, 3}, []int{16, 0, -16}},
		{"вышедший хуже всех", []int{1000, 1000, 1000}, []int{1, 0, 2}, []int{16, -16, 0}},
		{"общее место", []int{1000, 1000, 1000}, []int{1, 1, 3}, []int{8, 8, -16}},
	} {
		got := eloDeltas(tc.ratings, tc.places)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, ожидалось %v", tc.name, got, tc.want)
		}
	}
}

func seatRec(id string, place int, score int, result string) SeatRec {
	return SeatRec{User: id, Place: place, Score: score, Result: result, BestStrk: 2}
}

func TestMatchUpdatesResultsAndRatings(t *testing.T) {
	s := testStore(t)
	a, b, c := newUser(t, s, "Анна"), newUser(t, s, "Борис"), newUser(t, s, "Вика")

	// против ботов: результат записывается, рейтинг не меняется
	s.RecordMatch(MatchRec{Players: 2, Rounds: 3, QPerRound: 2, Seats: []SeatRec{seatRec(a, 1, 500, "win")}})
	// двое людей: рейтинг меняется
	s.RecordMatch(MatchRec{Players: 2, Rounds: 3, QPerRound: 2, Seats: []SeatRec{seatRec(a, 1, 600, "win"), seatRec(b, 2, 400, "lose")}})
	// трое, один вышел
	s.RecordMatch(MatchRec{Players: 3, Rounds: 3, QPerRound: 2, Seats: []SeatRec{seatRec(b, 1, 300, "win"), seatRec(a, 2, 200, "lose"), seatRec(c, 0, 900, "lose")}})
	s.Flush()

	pa, _ := s.Profile(a)
	if pa.Games != 3 || pa.Wins != 2 || pa.Losses != 1 || pa.BestScore != 600 || pa.Points != 1300 || pa.BestStreak != 2 {
		t.Errorf("Анна: %+v", pa)
	}
	if pa.RatedGames != 2 || len(pa.Recent) != 3 || pa.Recent[0].Place != 2 || pa.Recent[2].Rated {
		t.Errorf("Анна: рейтинговые партии %d, последние %+v", pa.RatedGames, pa.Recent)
	}
	pc, _ := s.Profile(c)
	if pc.Rating >= startRating || pc.Rank == 0 {
		t.Errorf("вышедший должен потерять рейтинг: %+v", pc)
	}
	sum := 0
	for _, id := range []string{a, b, c} {
		p, _ := s.Profile(id)
		sum += p.Rating - startRating
	}
	if sum < -2 || sum > 2 {
		t.Errorf("рейтинг должен только перетекать между игроками, суммарно %+d", sum)
	}

	top, err := s.Top(10)
	if err != nil || len(top) != 3 {
		t.Fatalf("топ: %v, %v", top, err)
	}
	for i := 1; i < len(top); i++ {
		if top[i-1].Rating < top[i].Rating {
			t.Errorf("топ не по убыванию: %+v", top)
		}
	}
	if p, _ := s.Profile(top[0].ID); p.Rank != 1 {
		t.Errorf("первый в топе имеет место %d", p.Rank)
	}

	// у того, кто играл только с ботами, места в рейтинге нет
	d := newUser(t, s, "Дима")
	s.RecordMatch(MatchRec{Players: 2, Seats: []SeatRec{seatRec(d, 1, 100, "win")}})
	s.Flush()
	if p, _ := s.Profile(d); p.Rank != 0 || p.Rating != startRating {
		t.Errorf("только игры с ботами: %+v", p)
	}
	if top, _ := s.Top(10); len(top) != 3 {
		t.Errorf("в топе только рейтинговые игроки: %+v", top)
	}
}

func TestProfileOfNobody(t *testing.T) {
	if _, err := testStore(t).Profile("nobody"); err != errNoUser {
		t.Errorf("ошибка %v", err)
	}
}

// ---- the game feeds the store -------------------------------------------

func storeHub(t *testing.T) (*Hub, *Store) {
	t.Helper()
	h := testHub(t)
	h.store = testStore(t)
	return h, h.store
}

func sum(t *testing.T, s *Store, q string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestMatchOfTwoPeopleIsRecorded(t *testing.T) {
	h, s := storeHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Борис")
	a.next()
	b.next()
	ua, ub := newUser(t, s, "Анна"), newUser(t, s, "Борис")
	a.must(h.Attach(a.id, ua, "Анна"))
	b.must(h.Attach(b.id, ub, "Борис"))

	a.must(h.CreateRoom(a.id, GameOpts{Rounds: 2, QPerRound: 2}))
	b.must(h.JoinRoom(b.id, a.nextScreen("room").Room))
	a.must(h.Start(a.id))
	done := make(chan *GameView, 2)
	for _, p := range []*player{a, b} {
		go func() {
			var g *GameView
			defer func() { done <- g }()
			g = p.rushToEnd(h.cfg)
		}()
	}
	if x, y := <-done, <-done; x == nil || y == nil {
		t.FailNow()
	}
	s.Flush()

	// 2 раунда × 2 вопроса × 2 человека, все ответили верно
	if n := sum(t, s, `SELECT SUM(asked) FROM question_stats`); n != 8 {
		t.Errorf("ответов в статистике вопросов %d, ожидалось 8", n)
	}
	if n := sum(t, s, `SELECT SUM(correct) FROM question_stats`); n != 8 {
		t.Errorf("верных ответов %d, ожидалось 8", n)
	}
	for _, id := range []string{ua, ub} {
		p, _ := s.Profile(id)
		if p.Games != 1 || p.Draws != 1 || p.Answered != 4 || p.Correct != 4 || p.BestStreak != 4 || p.RatedGames != 1 || p.Rating != startRating {
			t.Errorf("%s: %+v", p.Name, p)
		}
		if len(p.Cats) != 2 {
			t.Errorf("%s: по темам %+v", p.Name, p.Cats)
		}
	}
}

func TestBotAnswersStayOutOfQuestionStats(t *testing.T) {
	h, s := storeHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	ua := newUser(t, s, "Анна")
	a.must(h.Attach(a.id, ua, "Анна"))
	a.must(h.Bot(a.id, GameOpts{Rounds: 2, QPerRound: 1, Bots: 3}))
	a.rushToEnd(h.cfg)
	s.Flush()

	if n := sum(t, s, `SELECT SUM(asked) FROM question_stats`); n != 2 {
		t.Errorf("в статистике вопросов должны быть только ответы человека: %d, ожидалось 2", n)
	}
	p, _ := s.Profile(ua)
	if p.Games != 1 || p.Answered != 2 || p.RatedGames != 0 || p.Rank != 0 || p.Rating != startRating {
		t.Errorf("игра с ботами: %+v", p)
	}
}

func TestOneProfileInTwoTabsDoesNotPlayItself(t *testing.T) {
	h, s := storeHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	b := connectPlayer(t, h, "player-bbbbbbbb", "Анна")
	a.next()
	b.next()
	u := newUser(t, s, "Анна")
	a.must(h.Attach(a.id, u, "Анна"))
	b.must(h.Attach(b.id, u, "Анна"))
	a.must(h.CreateRoom(a.id, GameOpts{Rounds: 1, QPerRound: 1}))
	b.must(h.JoinRoom(b.id, a.nextScreen("room").Room))
	a.must(h.Start(a.id))
	done := make(chan *GameView, 2)
	for _, p := range []*player{a, b} {
		go func() {
			var g *GameView
			defer func() { done <- g }()
			g = p.rushToEnd(h.cfg)
		}()
	}
	<-done
	<-done
	s.Flush()

	if p, _ := s.Profile(u); p.Games != 0 || p.Answered != 0 {
		t.Errorf("игра с самим собой не должна считаться: %+v", p)
	}
	if n := sum(t, s, `SELECT SUM(asked) FROM question_stats`); n != 2 {
		t.Errorf("вопрос всё же задавали двум людям: %d", n)
	}
}

func TestRenameUpdatesTheProfile(t *testing.T) {
	h, s := storeHub(t)
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	u := newUser(t, s, "Анна")
	a.must(h.Attach(a.id, u, "Анна"))
	a.must(h.Rename(a.id, "  Анюта "))
	s.Flush()
	if p, _ := s.Profile(u); p.Name != "Анюта" {
		t.Errorf("имя в профиле %q", p.Name)
	}
}

func TestPlayWithoutADatabase(t *testing.T) {
	h := testHub(t) // h.store == nil
	a := connectPlayer(t, h, "player-aaaaaaaa", "Анна")
	a.next()
	a.must(h.Bot(a.id, GameOpts{Rounds: 1, QPerRound: 1}))
	if g := a.rushToEnd(h.cfg); g == nil {
		t.Fatal("игра без базы не дошла до конца")
	}
}
