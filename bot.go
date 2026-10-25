package main

import (
	"math/rand/v2"
	"slices"
	"time"
)

var botNames = []string{
	"Алиса", "Борис", "Виктор", "Глеб", "Дарья", "Елена", "Жора", "Зоя",
	"Игорь", "Кира", "Леонид", "Мария", "Никита", "Ольга", "Платон", "Рита",
}

// botLevel is how well and how fast a bot plays: the chance of a right answer
// lies in [accLo, accHi], the reaction time in [delayLo, delayHi] of the
// question time.
type botLevel struct {
	accLo, accHi     float64
	delayLo, delayHi float64
}

var (
	botEasy   = botLevel{0.35, 0.50, 0.25, 0.85}
	botNormal = botLevel{0.55, 0.80, 0.12, 0.65}
	botHard   = botLevel{0.80, 0.95, 0.08, 0.40}
)

var botLevels = map[string]botLevel{"easy": botEasy, "normal": botNormal, "hard": botHard}

// newBotSeats seats n bots, each with a name of its own that no human at the
// table has.
func newBotSeats(n int, l botLevel, taken []string) []*seat {
	names := slices.Clone(botNames)
	rand.Shuffle(len(names), func(i, j int) { names[i], names[j] = names[j], names[i] })
	var seats []*seat
	for _, name := range names {
		if len(seats) == n {
			break
		}
		if slices.Contains(taken, "🤖 "+name) {
			continue
		}
		seats = append(seats, newBotSeat(l))
		seats[len(seats)-1].name = "🤖 " + name
	}
	return seats
}

func newBotSeat(l botLevel) *seat {
	return &seat{
		name:      "🤖 " + botNames[rand.IntN(len(botNames))],
		bot:       true,
		accuracy:  l.accLo + rand.Float64()*(l.accHi-l.accLo),
		delayLo:   l.delayLo,
		delayHi:   l.delayHi,
		connected: true,
	}
}

// pickOption returns the bot's answer: the right option with probability
// accuracy, otherwise a random wrong one.
func (s *seat) pickOption(correct int) int {
	if rand.Float64() < s.accuracy {
		return correct
	}
	wrong := rand.IntN(3)
	if wrong >= correct {
		wrong++
	}
	return wrong
}

// botDelay is a random reaction time between lo and hi of the given window.
func botDelay(window time.Duration, lo, hi float64) time.Duration {
	return time.Duration(float64(window) * (lo + rand.Float64()*(hi-lo)))
}
