package main

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

func TestEmbeddedBankIsValid(t *testing.T) {
	bank, err := LoadBank(embeddedData, "data", DefaultConfig().QPerRound)
	if err != nil {
		t.Fatalf("вшитая база вопросов невалидна:\n%v", err)
	}
	if len(bank.Cats) < 8 {
		t.Errorf("категорий мало: %d", len(bank.Cats))
	}
	total := 0
	for _, c := range bank.Cats {
		// 6 раундов × 3 вопроса: в категории должно хватать на разные матчи подряд
		if len(c.Questions) < 15 {
			t.Errorf("в категории %q всего %d вопросов, нужно хотя бы 15", c.ID, len(c.Questions))
		}
		total += len(c.Questions)
	}
	t.Logf("%d категорий, %d вопросов", len(bank.Cats), total)
}

// Сторонние вопросы должны быть помечены "src", а их происхождение и условия
// использования описаны в data/NOTICE.md: Open Trivia DB — CC BY-SA 4.0, вопросы ЧГК —
// только для личного использования, без распространения.
func TestThirdPartyQuestionsAreAttributed(t *testing.T) {
	bank, err := LoadBank(embeddedData, "data", DefaultConfig().QPerRound)
	if err != nil {
		t.Fatal(err)
	}
	// источник -> что обязано упоминаться в data/NOTICE.md
	required := map[string]string{
		"opentdb":  "CC BY-SA 4.0",
		"chgk":     "только для личного использования",
		"wikidata": "CC0",
		"siq":      "Своя игра",
	}
	used := map[string]int{}
	for _, c := range bank.Cats {
		for i, q := range c.Questions {
			if q.Source == "" {
				continue
			}
			if _, ok := required[q.Source]; !ok {
				t.Errorf("категория %q, вопрос #%d: неизвестный источник %q", c.ID, i+1, q.Source)
			}
			used[q.Source]++
		}
	}
	notice, err := os.ReadFile("data/NOTICE.md")
	if err != nil {
		t.Fatalf("нет data/NOTICE.md: %v", err)
	}
	for src, n := range used {
		if want, ok := required[src]; ok && !strings.Contains(string(notice), want) {
			t.Errorf("в базе %d вопросов с источником %q, а в data/NOTICE.md нет фразы %q", n, src, want)
		}
	}
}

// Вопросы-шаблоны («Какая из этих рек самая длинная?») повторяют текст с разными
// вариантами ответа: это разные вопросы, и статистика ведётся по каждому отдельно.
func TestSameTextWithOtherOptionsIsAnotherQuestion(t *testing.T) {
	a := &Question{Text: "Какая из этих рек самая длинная?", Correct: "Волга", Wrong: []string{"Ока", "Кама", "Дон"}}
	b := &Question{Text: a.Text, Correct: "Нил", Wrong: []string{"Ока", "Кама", "Дон"}}
	c := &Question{Text: a.Text, Correct: "Волга", Wrong: []string{"Дон", "Кама", "Ока"}} // те же варианты в другом порядке
	if questionKey("g", a) == questionKey("g", b) {
		t.Error("разные варианты ответа должны давать разные ключи")
	}
	if questionKey("g", a) != questionKey("g", c) {
		t.Error("порядок неверных вариантов не должен менять ключ")
	}
	bank := fstest.MapFS{"d/x.json": {Data: []byte(`{"id":"x","name":"X","icon":"x","questions":[
		{"q":"?","correct":"a","wrong":["b","c","d"]},{"q":"?","correct":"e","wrong":["b","c","d"]},{"q":"??","correct":"a","wrong":["b","c","d"]}]}`)}}
	if _, err := LoadBank(bank, "d", 3); err != nil {
		t.Errorf("один текст с разными ответами должен загружаться: %v", err)
	}
}

func TestNewAskedShufflesAndKeepsAnswer(t *testing.T) {
	q := &Question{Text: "?", Correct: "верный", Wrong: []string{"а", "б", "в"}}
	positions := map[int]bool{}
	for range 200 {
		a := newAsked(q)
		if len(a.Options) != 4 || a.Options[a.Answer] != "верный" {
			t.Fatalf("правильный ответ потерялся: %+v", a)
		}
		positions[a.Answer] = true
	}
	if len(positions) != 4 {
		t.Errorf("правильный ответ встречался только на позициях %v", positions)
	}
}

func TestLoadBankRejectsBadData(t *testing.T) {
	good := `{"id":"x","name":"X","icon":"❓","questions":[` +
		`{"q":"1?","correct":"a","wrong":["b","c","d"]},` +
		`{"q":"2?","correct":"a","wrong":["b","c","d"]},` +
		`{"q":"3?","correct":"a","wrong":["b","c","d"]}]}`
	cases := map[string]struct {
		json string
		want string
	}{
		"ok":               {good, ""},
		"битый json":       {`{`, "unexpected end"},
		"мало вопросов":    {strings.Replace(good, `,{"q":"3?","correct":"a","wrong":["b","c","d"]}`, "", 1), "минимум 3"},
		"два неверных":     {strings.Replace(good, `"wrong":["b","c","d"]}]`, `"wrong":["b","c"]}]`, 1), "ровно 3"},
		"повтор варианта":  {strings.Replace(good, `"wrong":["b","c","d"]}]`, `"wrong":["A","c","d"]}]`, 1), "повторяющийся"},
		"дубликат вопроса": {strings.Replace(good, `"2?"`, `"1?"`, 1), "дубликат"},
		"пустой id":        {strings.Replace(good, `"id":"x"`, `"id":""`, 1), "id и name"},
		"пустой вариант":   {strings.Replace(good, `"wrong":["b","c","d"]}]`, `"wrong":["b","c"," "]}]`, 1), "пустой вариант"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{"d/x.json": {Data: []byte(tc.json)}}
			_, err := LoadBank(fsys, "d", 3)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("неожиданная ошибка: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("ожидалась ошибка с %q, получили %v", tc.want, err)
			}
		})
	}

	dup := fstest.MapFS{"d/a.json": {Data: []byte(good)}, "d/b.json": {Data: []byte(good)}}
	if _, err := LoadBank(dup, "d", 3); err == nil || !strings.Contains(err.Error(), "уже используется") {
		t.Errorf("два файла с одинаковым id должны отклоняться, err=%v", err)
	}
	if _, err := LoadBank(fstest.MapFS{}, "d", 3); err == nil {
		t.Error("пустая папка должна давать ошибку")
	}
}

func TestBankWithout(t *testing.T) {
	q := func(text, src string) Question {
		return Question{Text: text, Correct: "a", Wrong: []string{"b", "c", "d"}, Source: src}
	}
	bank := &Bank{Cats: []*Category{
		{ID: "mixed", Name: "M", Questions: []Question{q("1?", ""), q("2?", "chgk"), q("3?", ""), q("4?", "opentdb")}},
		{ID: "all-chgk", Name: "C", Questions: []Question{q("5?", "chgk"), q("6?", "chgk"), q("7?", "chgk")}},
	}}
	if n := bank.CountFrom("chgk"); n != 4 {
		t.Errorf("вопросов chgk: %d, ожидали 4", n)
	}
	got := bank.Without(2, "chgk")
	if len(got.Cats) != 1 || got.Cats[0].ID != "mixed" || len(got.Cats[0].Questions) != 3 {
		t.Fatalf("после исключения: %+v", got.Cats)
	}
	if got.CountFrom("chgk") != 0 {
		t.Error("вопросы исключённого источника остались")
	}
	if len(bank.Cats[0].Questions) != 4 {
		t.Error("исходная база не должна меняться")
	}
}
