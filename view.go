package main

import "time"

// Everything the browser receives is a full snapshot of "what should be on
// screen now", so a reconnecting client never has to replay missed events.

// ClientState is the payload of every SSE "state" event.
type ClientState struct {
	Screen   string     `json:"screen"` // menu | queue | room | game
	Name     string     `json:"name"`
	Room     string     `json:"room,omitempty"`
	Rules    *RulesView `json:"rules,omitempty"`    // the room's rules, on the "room" screen
	RoomCats []CatView  `json:"roomCats,omitempty"` // ...and its categories, when not all of them are played
	Lobby    *LobbyView `json:"lobby,omitempty"`    // ...and who is in it
	Game     *GameView  `json:"game,omitempty"`
}

// RulesView is what a player can choose when creating a room.
type RulesView struct {
	Seconds   int  `json:"seconds"`  // time to answer one question
	Speed     bool `json:"speed"`    // a faster right answer is worth more
	Tiebreak  bool `json:"tiebreak"` // level scores are settled by extra questions
	Hints     bool `json:"hints"`    // every player has three hints
	Rounds    int  `json:"rounds"`
	QPerRound int  `json:"qPerRound"`
}

func rulesView(cfg Config) RulesView {
	return RulesView{
		Seconds: int(cfg.QuestionTime / time.Second), Speed: cfg.SpeedScore, Tiebreak: cfg.Tiebreak, Hints: cfg.Hints,
		Rounds: cfg.Rounds, QPerRound: cfg.QPerRound,
	}
}

// LobbyView is the list of players in a room that has not started yet.
type LobbyView struct {
	Players  []LobbyPlayer `json:"players"`
	Host     bool          `json:"host"`     // I made the room and I start the game
	Max      int           `json:"max"`      // seats in the room, bots included
	CanStart bool          `json:"canStart"` // enough players are here
}

type LobbyPlayer struct {
	Name      string `json:"name"`
	Bot       bool   `json:"bot"`
	Owner     bool   `json:"owner"`
	Me        bool   `json:"me"`
	Connected bool   `json:"connected"`
}

type CatView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

// PlayerView is one of the players of a match; a game view lists them all in
// seat order.
type PlayerView struct {
	Name      string `json:"name"`
	Score     int    `json:"score"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
	Left      bool   `json:"left"`     // gone for good
	Answered  bool   `json:"answered"` // gave an answer to the current question
	Answer    int    `json:"answer"`   // the chosen option: mine at once, the others' once revealed; -1 otherwise
	Points    int    `json:"points"`   // won with the current question, once revealed
	Rematch   bool   `json:"rematch"`  // wants to play again
	Place     int    `json:"place"`    // final phase; 0: left the game
}

// RoundView is one row of the scoreboard. Res holds one list per player (in
// seat order), one entry per question: 0 not played yet, 1 right, 2 wrong.
type RoundView struct {
	Cat *CatView `json:"cat"`
	Res [][]int  `json:"res"`
}

type QuestionView struct {
	Seq     int      `json:"seq"`
	Text    string   `json:"text"`
	Options []string `json:"options"`
}

// HintsView is what the player has left of the hints; Hidden are the options
// crossed out for the current question.
type HintsView struct {
	Fifty  bool  `json:"fifty"`
	Second bool  `json:"second"`
	Extra  bool  `json:"extra"`
	Armed  bool  `json:"armed"` // the second chance is switched on
	Hidden []int `json:"hidden"`
}

type GameView struct {
	Phase     string `json:"phase"`
	Round     int    `json:"round"`  // 1-based
	Rounds    int    `json:"rounds"` // total
	QIndex    int    `json:"qIndex"` // 1-based within the round
	QPerRound int    `json:"qPerRound"`
	Double    bool   `json:"double"`   // last round: points are doubled
	Tiebreak  bool   `json:"tiebreak"` // the scores were level: extra questions are asked or were asked

	Rules   RulesView    `json:"rules"`
	Players []PlayerView `json:"players"`
	Me      int          `json:"me"` // my index in Players
	Board   []RoundView  `json:"board"`

	Cat *CatView `json:"cat,omitempty"`

	// pick phase
	Choices     []CatView `json:"choices,omitempty"`
	Chooser     int       `json:"chooser"` // index in Players, -1 outside the pick phase
	IPick       bool      `json:"iPick"`
	ChooserName string    `json:"chooserName,omitempty"`
	Paused      bool      `json:"paused"`             // the wheel is frozen, TimeLeftMs stands still
	IPaused     bool      `json:"iPaused"`            // ...by me
	PausedBy    string    `json:"pausedBy,omitempty"` // ...by this player

	// pick and question phases
	TimeLeftMs  int64 `json:"timeLeftMs"`
	TimeTotalMs int64 `json:"timeTotalMs"`

	// question and reveal phases
	Question *QuestionView `json:"question,omitempty"`
	Watching bool          `json:"watching"`         // a tie-break I am not in
	Hints    *HintsView    `json:"hints,omitempty"`  // when the room has hints
	Notice   string        `json:"notice,omitempty"` // e.g. who gave everybody more time

	// reveal phase
	Correct int `json:"correct"` // -1 until revealed

	// final phase
	Result     string `json:"result,omitempty"` // win | lose | draw (shared first place)
	Order      []int  `json:"order,omitempty"`  // indices in Players, from first place down
	Forfeit    bool   `json:"forfeit"`          // too many players left, the one who stayed wins
	CanRematch bool   `json:"canRematch"`       // enough players are left for one more game
}
