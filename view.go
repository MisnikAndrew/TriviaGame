package main

// Everything the browser receives is a full snapshot of "what should be on
// screen now", so a reconnecting client never has to replay missed events.

// ClientState is the payload of every SSE "state" event.
type ClientState struct {
	Screen string    `json:"screen"` // menu | queue | room | game
	Name   string    `json:"name"`
	Room   string    `json:"room,omitempty"`
	Game   *GameView `json:"game,omitempty"`
}

type CatView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

type PlayerView struct {
	Name      string `json:"name"`
	Score     int    `json:"score"`
	Connected bool   `json:"connected"`
	Bot       bool   `json:"bot"`
}

// RoundView is one row of the scoreboard. Me/Opp hold one entry per
// question: 0 not played yet, 1 right, 2 wrong.
type RoundView struct {
	Cat *CatView `json:"cat"`
	Me  []int    `json:"me"`
	Opp []int    `json:"opp"`
}

type QuestionView struct {
	Seq     int      `json:"seq"`
	Text    string   `json:"text"`
	Options []string `json:"options"`
}

type GameView struct {
	Phase     string `json:"phase"`
	Round     int    `json:"round"`  // 1-based
	Rounds    int    `json:"rounds"` // total
	QIndex    int    `json:"qIndex"` // 1-based within the round
	QPerRound int    `json:"qPerRound"`
	Double    bool   `json:"double"` // last round: points are doubled

	Me    PlayerView  `json:"me"`
	Opp   PlayerView  `json:"opp"`
	Board []RoundView `json:"board"`

	Cat *CatView `json:"cat,omitempty"`

	// pick phase
	Choices     []CatView `json:"choices,omitempty"`
	IPick       bool      `json:"iPick"`
	ChooserName string    `json:"chooserName,omitempty"`

	// pick and question phases
	TimeLeftMs  int64 `json:"timeLeftMs"`
	TimeTotalMs int64 `json:"timeTotalMs"`

	// question and reveal phases
	Question    *QuestionView `json:"question,omitempty"`
	MyAnswer    int           `json:"myAnswer"` // -1: none
	OppAnswered bool          `json:"oppAnswered"`

	// reveal phase
	Correct   int `json:"correct"`   // -1 until revealed
	OppAnswer int `json:"oppAnswer"` // -1: none
	MyPoints  int `json:"myPoints"`
	OppPoints int `json:"oppPoints"`

	// final phase
	Result     string `json:"result,omitempty"` // win | lose | draw
	Forfeit    bool   `json:"forfeit"`          // the opponent left, the win is automatic
	OppLeft    bool   `json:"oppLeft"`
	RematchMe  bool   `json:"rematchMe"`
	RematchOpp bool   `json:"rematchOpp"`
}
