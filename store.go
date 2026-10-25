package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // SQLite without cgo
)

const (
	startRating   = 1000
	ratingK       = 32 // how far one match can move a rating
	storeQueueLen = 4096
	recentMatches = 10
)

var errBadLogin = errors.New("профиль не найден или код не подошёл")

// Store keeps players and everything we count about them in SQLite. Writes
// that the game produces (answers, finished matches) go through a queue and
// one worker goroutine, so a match never waits for the disk.
type Store struct {
	db  *sql.DB
	ops chan storeOp
	end chan struct{}
}

type storeOp struct {
	fn    func(*sql.Tx) error
	flush chan struct{} // closed once everything queued before it is written
}

var memDBs atomic.Int64

// OpenStore opens (and, the first time, creates) the database at path;
// ":memory:" gives a private in-memory one, for tests.
func OpenStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	if path == ":memory:" {
		dsn = fmt.Sprintf("file:triviamem%d?mode=memory&cache=shared&_pragma=foreign_keys(1)", memDBs.Add(1))
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if path == ":memory:" {
		db.SetMaxOpenConns(1) // a shared in-memory database lives as long as one connection does
	}
	s := &Store{db: db, ops: make(chan storeOp, storeQueueLen), end: make(chan struct{})}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("база %s: %w", path, err)
	}
	go s.worker()
	return s, nil
}

const schemaV1 = `
CREATE TABLE users (
	id           TEXT PRIMARY KEY,
	secret_hash  BLOB NOT NULL,
	name         TEXT NOT NULL,
	rating       INTEGER NOT NULL DEFAULT 1000,
	rated_games  INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	last_seen    INTEGER NOT NULL
);
CREATE INDEX users_rating ON users (rating DESC) WHERE rated_games > 0;

CREATE TABLE user_stats (
	user_id        TEXT PRIMARY KEY REFERENCES users (id),
	games          INTEGER NOT NULL DEFAULT 0,
	wins           INTEGER NOT NULL DEFAULT 0,
	draws          INTEGER NOT NULL DEFAULT 0,
	losses         INTEGER NOT NULL DEFAULT 0,
	answered       INTEGER NOT NULL DEFAULT 0,
	correct        INTEGER NOT NULL DEFAULT 0,
	points         INTEGER NOT NULL DEFAULT 0,
	best_score     INTEGER NOT NULL DEFAULT 0,
	best_streak    INTEGER NOT NULL DEFAULT 0,
	fastest_ms     INTEGER NOT NULL DEFAULT 0,
	tiebreaks_won  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE user_category_stats (
	user_id  TEXT NOT NULL REFERENCES users (id),
	cat_id   TEXT NOT NULL,
	asked    INTEGER NOT NULL DEFAULT 0,
	correct  INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (user_id, cat_id)
);

CREATE TABLE question_stats (
	key      TEXT PRIMARY KEY,
	cat_id   TEXT NOT NULL,
	text     TEXT NOT NULL,
	asked    INTEGER NOT NULL DEFAULT 0,
	correct  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE matches (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	finished_at  INTEGER NOT NULL,
	players      INTEGER NOT NULL,
	rounds       INTEGER NOT NULL,
	q_per_round  INTEGER NOT NULL,
	speed        INTEGER NOT NULL,
	rated        INTEGER NOT NULL
);

CREATE TABLE match_players (
	match_id  INTEGER NOT NULL REFERENCES matches (id),
	user_id   TEXT NOT NULL REFERENCES users (id),
	place     INTEGER NOT NULL,
	result    TEXT NOT NULL,
	score     INTEGER NOT NULL,
	delta     INTEGER NOT NULL,
	PRIMARY KEY (match_id, user_id)
);
CREATE INDEX match_players_user ON match_players (user_id, match_id DESC);
`

// migrate brings the schema up to date; PRAGMA user_version says how far it is.
func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	if v >= 1 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(schemaV1); err != nil {
		return err
	}
	if _, err := tx.Exec(`PRAGMA user_version = 1`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) worker() {
	defer close(s.end)
	for op := range s.ops {
		if op.fn != nil {
			if err := s.inTx(op.fn); err != nil {
				log.Printf("база: запись не удалась: %v", err)
			}
		}
		if op.flush != nil {
			close(op.flush)
		}
	}
}

func (s *Store) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// enqueue never blocks the caller: when the disk cannot keep up, the record is dropped.
func (s *Store) enqueue(fn func(*sql.Tx) error) {
	if s == nil {
		return
	}
	select {
	case s.ops <- storeOp{fn: fn}:
	default:
		log.Print("база: очередь записи переполнена, запись пропущена")
	}
}

// Flush waits until everything queued so far is written.
func (s *Store) Flush() {
	if s == nil {
		return
	}
	done := make(chan struct{})
	s.ops <- storeOp{flush: done}
	<-done
}

// Close writes what is left in the queue and closes the database.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	close(s.ops)
	<-s.end
	return s.db.Close()
}

// ---- users ---------------------------------------------------------------

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// CreateUser registers a player. Whoever holds the returned secret is that
// player; only its hash is stored.
func (s *Store) CreateUser(name string) (id, secret string, err error) {
	id, secret = randomHex(8), randomHex(16)
	now := time.Now().Unix()
	err = s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO users (id, secret_hash, name, created_at, last_seen) VALUES (?, ?, ?, ?, ?)`,
			id, hashSecret(secret), name, now, now); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO user_stats (user_id) VALUES (?)`, id)
		return err
	})
	return id, secret, err
}

// Login checks the secret and returns the player's name.
func (s *Store) Login(id, secret string) (name string, err error) {
	var hash []byte
	err = s.db.QueryRow(`SELECT secret_hash, name FROM users WHERE id = ?`, id).Scan(&hash, &name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errBadLogin
	}
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare(hash, hashSecret(secret)) != 1 {
		return "", errBadLogin
	}
	s.db.Exec(`UPDATE users SET last_seen = ? WHERE id = ?`, time.Now().Unix(), id)
	return name, nil
}

func (s *Store) SetName(id, name string) {
	s.enqueue(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE users SET name = ? WHERE id = ?`, name, id)
		return err
	})
}

// ---- what the game reports -----------------------------------------------

// AnswerRec is one answer a human gave to a question.
type AnswerRec struct {
	User  string // "" for a player without a profile
	Cat   string
	Key   string // questionKey
	Text  string
	Right bool
	Ms    int // how long the player thought
}

// RecordAnswers counts the answers: for the question, and for the player.
func (s *Store) RecordAnswers(recs []AnswerRec) {
	s.enqueue(func(tx *sql.Tx) error {
		for _, r := range recs {
			right := 0
			if r.Right {
				right = 1
			}
			if _, err := tx.Exec(`INSERT INTO question_stats (key, cat_id, text, asked, correct) VALUES (?, ?, ?, 1, ?)
				ON CONFLICT (key) DO UPDATE SET asked = asked + 1, correct = correct + excluded.correct`,
				r.Key, r.Cat, r.Text, right); err != nil {
				return err
			}
			if r.User == "" {
				continue
			}
			if _, err := tx.Exec(`INSERT INTO user_category_stats (user_id, cat_id, asked, correct) VALUES (?, ?, 1, ?)
				ON CONFLICT (user_id, cat_id) DO UPDATE SET asked = asked + 1, correct = correct + excluded.correct`,
				r.User, r.Cat, right); err != nil {
				return err
			}
			fastest := 0
			if r.Right {
				fastest = max(r.Ms, 1)
			}
			if _, err := tx.Exec(`UPDATE user_stats SET answered = answered + 1, correct = correct + ?,
				fastest_ms = CASE WHEN ? > 0 AND (fastest_ms = 0 OR ? < fastest_ms) THEN ? ELSE fastest_ms END
				WHERE user_id = ?`, right, fastest, fastest, fastest, r.User); err != nil {
				return err
			}
		}
		return nil
	})
}

// SeatRec is how one human player ended a match.
type SeatRec struct {
	User     string
	Place    int // 0: left the match
	Score    int
	Result   string // win | draw | lose
	BestStrk int    // longest run of right answers
	TieWon   bool
}

// MatchRec is a finished match.
type MatchRec struct {
	Players   int // seats, bots included
	Rounds    int
	QPerRound int
	Speed     bool
	Seats     []SeatRec // the humans that have a profile
}

// RecordMatch updates the players' results and, when at least two of them
// took part, their ratings.
func (s *Store) RecordMatch(m MatchRec) {
	if len(m.Seats) == 0 {
		return
	}
	s.enqueue(func(tx *sql.Tx) error {
		rated := len(m.Seats) >= 2
		ratings := make([]int, len(m.Seats))
		places := make([]int, len(m.Seats))
		for i, sr := range m.Seats {
			if err := tx.QueryRow(`SELECT rating FROM users WHERE id = ?`, sr.User).Scan(&ratings[i]); err != nil {
				return err
			}
			places[i] = sr.Place
		}
		deltas := make([]int, len(m.Seats))
		if rated {
			deltas = eloDeltas(ratings, places)
		}
		res, err := tx.Exec(`INSERT INTO matches (finished_at, players, rounds, q_per_round, speed, rated) VALUES (?, ?, ?, ?, ?, ?)`,
			time.Now().Unix(), m.Players, m.Rounds, m.QPerRound, boolInt(m.Speed), boolInt(rated))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		for i, sr := range m.Seats {
			if _, err := tx.Exec(`INSERT INTO match_players (match_id, user_id, place, result, score, delta) VALUES (?, ?, ?, ?, ?, ?)`,
				id, sr.User, sr.Place, sr.Result, sr.Score, deltas[i]); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE users SET rating = rating + ?, rated_games = rated_games + ? WHERE id = ?`,
				deltas[i], boolInt(rated), sr.User); err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE user_stats SET games = games + 1,
				wins = wins + ?, draws = draws + ?, losses = losses + ?, points = points + ?,
				best_score = MAX(best_score, ?), best_streak = MAX(best_streak, ?), tiebreaks_won = tiebreaks_won + ?
				WHERE user_id = ?`,
				boolInt(sr.Result == "win"), boolInt(sr.Result == "draw"), boolInt(sr.Result == "lose"), sr.Score,
				sr.Score, sr.BestStrk, boolInt(sr.TieWon), sr.User); err != nil {
				return err
			}
		}
		return nil
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// eloDeltas is the change of every player's rating after one match: each pair
// of players plays a little Elo game decided by their places (0 means the
// player left, which is worse than any place). The more players, the less a
// single pairing weighs, so a match for eight moves ratings no further than a duel.
func eloDeltas(ratings, places []int) []int {
	n := len(ratings)
	rank := func(p int) int {
		if p == 0 {
			return math.MaxInt
		}
		return p
	}
	out := make([]int, n)
	for i := range ratings {
		sum := 0.0
		for j := range ratings {
			if i == j {
				continue
			}
			actual := 0.5
			switch {
			case rank(places[i]) < rank(places[j]):
				actual = 1
			case rank(places[i]) > rank(places[j]):
				actual = 0
			}
			expected := 1 / (1 + math.Pow(10, float64(ratings[j]-ratings[i])/400))
			sum += actual - expected
		}
		out[i] = int(math.Round(ratingK / float64(n-1) * sum))
	}
	return out
}

// ---- what the browser shows ------------------------------------------------

type CatStat struct {
	Cat     string `json:"cat"`
	Asked   int    `json:"asked"`
	Correct int    `json:"correct"`
}

type MatchLine struct {
	At      int64  `json:"at"`
	Place   int    `json:"place"`
	Result  string `json:"result"`
	Players int    `json:"players"`
	Score   int    `json:"score"`
	Delta   int    `json:"delta"`
	Rated   bool   `json:"rated"`
}

type Profile struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Rating     int         `json:"rating"`
	Rank       int         `json:"rank"` // place among the rated players, 0: no rated game yet
	RatedGames int         `json:"ratedGames"`
	Since      int64       `json:"since"`
	Games      int         `json:"games"`
	Wins       int         `json:"wins"`
	Draws      int         `json:"draws"`
	Losses     int         `json:"losses"`
	Answered   int         `json:"answered"`
	Correct    int         `json:"correct"`
	Points     int         `json:"points"`
	BestScore  int         `json:"bestScore"`
	BestStreak int         `json:"bestStreak"`
	FastestMs  int         `json:"fastestMs"` // quickest right answer, 0: none yet
	TieWon     int         `json:"tiebreaksWon"`
	Cats       []CatStat   `json:"cats"`
	Recent     []MatchLine `json:"recent"`
}

var errNoUser = errors.New("игрок не найден")

func (s *Store) Profile(id string) (*Profile, error) {
	p := &Profile{ID: id, Cats: []CatStat{}, Recent: []MatchLine{}}
	err := s.db.QueryRow(`SELECT u.name, u.rating, u.rated_games, u.created_at,
			st.games, st.wins, st.draws, st.losses, st.answered, st.correct, st.points,
			st.best_score, st.best_streak, st.fastest_ms, st.tiebreaks_won
		FROM users u JOIN user_stats st ON st.user_id = u.id WHERE u.id = ?`, id).Scan(
		&p.Name, &p.Rating, &p.RatedGames, &p.Since,
		&p.Games, &p.Wins, &p.Draws, &p.Losses, &p.Answered, &p.Correct, &p.Points,
		&p.BestScore, &p.BestStreak, &p.FastestMs, &p.TieWon)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNoUser
	}
	if err != nil {
		return nil, err
	}
	if p.RatedGames > 0 {
		if err := s.db.QueryRow(`SELECT COUNT(*) + 1 FROM users WHERE rated_games > 0 AND rating > ?`, p.Rating).Scan(&p.Rank); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.Query(`SELECT cat_id, asked, correct FROM user_category_stats WHERE user_id = ? ORDER BY asked DESC, cat_id`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c CatStat
		if err := rows.Scan(&c.Cat, &c.Asked, &c.Correct); err != nil {
			rows.Close()
			return nil, err
		}
		p.Cats = append(p.Cats, c)
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT m.finished_at, mp.place, mp.result, m.players, mp.score, mp.delta, m.rated
		FROM match_players mp JOIN matches m ON m.id = mp.match_id
		WHERE mp.user_id = ? ORDER BY m.id DESC LIMIT ?`, id, recentMatches)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var l MatchLine
		var rated int
		if err := rows.Scan(&l.At, &l.Place, &l.Result, &l.Players, &l.Score, &l.Delta, &rated); err != nil {
			return nil, err
		}
		l.Rated = rated == 1
		p.Recent = append(p.Recent, l)
	}
	return p, rows.Err()
}

type TopEntry struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Rating     int    `json:"rating"`
	RatedGames int    `json:"ratedGames"`
	Wins       int    `json:"wins"`
	Games      int    `json:"games"`
}

// Top lists the best rated players, the best first.
func (s *Store) Top(limit int) ([]TopEntry, error) {
	rows, err := s.db.Query(`SELECT u.id, u.name, u.rating, u.rated_games, st.wins, st.games
		FROM users u JOIN user_stats st ON st.user_id = u.id
		WHERE u.rated_games > 0 ORDER BY u.rating DESC, u.rated_games DESC, u.created_at LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TopEntry{}
	for rows.Next() {
		var t TopEntry
		if err := rows.Scan(&t.ID, &t.Name, &t.Rating, &t.RatedGames, &t.Wins, &t.Games); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// QuestionStat says how often a question was answered and how often right.
func (s *Store) QuestionStat(key string) (asked, correct int) {
	s.db.QueryRow(`SELECT asked, correct FROM question_stats WHERE key = ?`, key).Scan(&asked, &correct)
	return asked, correct
}
