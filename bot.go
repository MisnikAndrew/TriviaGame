package main

import (
	"math/rand/v2"
	"time"
)

var botNames = []string{
	"Алиса", "Борис", "Виктор", "Глеб", "Дарья", "Елена", "Жора", "Зоя",
	"Игорь", "Кира", "Леонид", "Мария", "Никита", "Ольга", "Платон", "Рита",
}

func newBotSeat() *seat {
	return &seat{
		name:      "🤖 " + botNames[rand.IntN(len(botNames))],
		bot:       true,
		accuracy:  0.55 + rand.Float64()*0.25, // 55..80% right answers
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
