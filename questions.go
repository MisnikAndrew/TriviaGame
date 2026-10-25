package main

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"path"
	"slices"
	"strings"
)

//go:embed data/*.json
var embeddedData embed.FS

// Question is one trivia question as stored in data/*.json: the correct
// answer is kept apart from the three wrong ones, the order of the four
// options is shuffled when the question is asked.
type Question struct {
	Text    string   `json:"q"`
	Correct string   `json:"correct"`
	Wrong   []string `json:"wrong"`
	// Source marks questions taken from a third-party database ("opentdb",
	// "chgk"; terms of use in data/NOTICE.md); empty for our own.
	Source string `json:"src,omitempty"`
}

// Category is one topic, loaded from a single JSON file.
type Category struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Icon      string     `json:"icon"`
	Questions []Question `json:"questions"`
}

// Bank is the whole question database.
type Bank struct {
	Cats []*Category
}

// Asked is a question prepared for a match: options already shuffled.
type Asked struct {
	Text    string
	Options []string
	Answer  int    // index of the correct option in Options
	Cat     string // id of the category it belongs to
	Key     string // identifies the question in the statistics
}

// questionKey names a question for the statistics; it survives restarts and
// changes only when the question's text or its options are edited. The options
// are part of the key because template questions ("Which of these rivers is the
// longest?") repeat their text with different answers.
func questionKey(cat string, q *Question) string {
	opts := append([]string{q.Correct}, q.Wrong...)
	slices.Sort(opts)
	sum := sha256.Sum256([]byte(cat + "\x00" + q.Text + "\x00" + strings.Join(opts, "\x1f")))
	return hex.EncodeToString(sum[:8])
}

func newAsked(q *Question) Asked {
	opts := append([]string{q.Correct}, q.Wrong...)
	rand.Shuffle(len(opts), func(i, j int) { opts[i], opts[j] = opts[j], opts[i] })
	a := Asked{Text: q.Text, Options: opts}
	for i, o := range opts {
		if o == q.Correct {
			a.Answer = i
		}
	}
	return a
}

// Without returns the bank minus the questions that come from the given
// sources; a category left with fewer than minPerCategory questions is dropped.
func (b *Bank) Without(minPerCategory int, sources ...string) *Bank {
	var out Bank
	for _, c := range b.Cats {
		kept := *c
		kept.Questions = nil
		for _, q := range c.Questions {
			if !slices.Contains(sources, q.Source) {
				kept.Questions = append(kept.Questions, q)
			}
		}
		if len(kept.Questions) >= minPerCategory {
			out.Cats = append(out.Cats, &kept)
		}
	}
	return &out
}

// Only returns the categories with the given ids that hold at least
// minPerCategory questions; it fails on an unknown id.
func (b *Bank) Only(minPerCategory int, ids []string) (*Bank, error) {
	var out Bank
	for _, c := range b.Cats {
		if slices.Contains(ids, c.ID) && len(c.Questions) >= minPerCategory {
			out.Cats = append(out.Cats, c)
		}
	}
	for _, id := range ids {
		if !slices.ContainsFunc(b.Cats, func(c *Category) bool { return c.ID == id }) {
			return nil, fmt.Errorf("неизвестная тема %q", id)
		}
	}
	return &out, nil
}

// CountFrom is the number of questions that come from the given source.
func (b *Bank) CountFrom(source string) int {
	n := 0
	for _, c := range b.Cats {
		for _, q := range c.Questions {
			if q.Source == source {
				n++
			}
		}
	}
	return n
}

// LoadBank reads and validates every *.json file from dir inside fsys.
// minPerCategory is the number of questions a category must hold to be playable.
func LoadBank(fsys fs.FS, dir string, minPerCategory int) (*Bank, error) {
	files, err := fs.Glob(fsys, path.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("в %q нет ни одного *.json файла с вопросами", dir)
	}

	var (
		bank Bank
		errs []error
		ids  = map[string]string{}
	)
	for _, f := range files {
		raw, err := fs.ReadFile(fsys, f)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var c Category
		if err := json.Unmarshal(raw, &c); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			continue
		}
		if err := c.validate(minPerCategory); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			continue
		}
		if prev, dup := ids[c.ID]; dup {
			errs = append(errs, fmt.Errorf("%s: id %q уже используется в %s", f, c.ID, prev))
			continue
		}
		ids[c.ID] = f
		bank.Cats = append(bank.Cats, &c)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return &bank, nil
}

func (c *Category) validate(minQuestions int) error {
	var errs []error
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Name) == "" {
		errs = append(errs, errors.New("у категории должны быть id и name"))
	}
	if len(c.Questions) < minQuestions {
		errs = append(errs, fmt.Errorf("категория %q: нужно минимум %d вопросов, есть %d", c.ID, minQuestions, len(c.Questions)))
	}
	seen := map[string]bool{}
	for i, q := range c.Questions {
		where := fmt.Sprintf("категория %q, вопрос #%d", c.ID, i+1)
		if strings.TrimSpace(q.Text) == "" {
			errs = append(errs, fmt.Errorf("%s: пустой текст", where))
		}
		if len(q.Wrong) != 3 {
			errs = append(errs, fmt.Errorf("%s: нужно ровно 3 неверных ответа, есть %d", where, len(q.Wrong)))
			continue
		}
		// the same text is fine for template questions as long as the options differ
		if k := questionKey(c.ID, &c.Questions[i]); seen[k] {
			errs = append(errs, fmt.Errorf("%s: дубликат вопроса %q", where, q.Text))
		} else {
			seen[k] = true
		}
		opts := map[string]bool{}
		for _, o := range append([]string{q.Correct}, q.Wrong...) {
			key := strings.ToLower(strings.TrimSpace(o))
			if key == "" {
				errs = append(errs, fmt.Errorf("%s: пустой вариант ответа", where))
			} else if opts[key] {
				errs = append(errs, fmt.Errorf("%s: повторяющийся вариант %q", where, o))
			}
			opts[key] = true
		}
	}
	return errors.Join(errs...)
}
