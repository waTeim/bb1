package engine

import (
	"time"

	"github.com/wat.im/bb1/oracle"
)

// Snapshot extracts the current engine state: EMA, sigma, tick buffer, and last update time.
// The returned slice of Tick is in chronological order (oldest first).
func (e *Engine) Snapshot() (ema, sigma float64, ticks []Tick, updated time.Time) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	ema = e.ema
	sigma = e.sigma
	updated = e.lastUpdated

	// collect ticks in chronological order
	n := e.count
	ticks = make([]Tick, n)
	for i := 0; i < n; i++ {
		idx := (e.pos - n + i + len(e.buffer)) % len(e.buffer)
		spot := e.buffer[idx]
		ts := e.tsBuf[idx]
		ticks[i] = Tick{
			Prices: oracle.PriceData{Spot: spot},
			TS:     ts,
		}
	}
	return
}

// Restore replaces the engine's state with a snapshot: tick buffer, EMA, sigma, and last update time.
func (e *Engine) Restore(saved []Tick, emaVal, sigmaVal float64, updated time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// trim to buffer size if necessary
	n := len(saved)
	if n > len(e.buffer) {
		saved = saved[n-len(e.buffer):]
		n = len(e.buffer)
	}

	e.count = n
	e.pos = n % len(e.buffer)

	// rebuild sums & buffer from Spot field
	e.sum = 0
	e.sumsq = 0
	for i := 0; i < n; i++ {
		spot := saved[i].Prices.Spot
		e.buffer[i] = spot
		e.tsBuf[i] = saved[i].TS
		e.sum += spot
		e.sumsq += spot * spot
	}

	e.ema = emaVal
	e.sigma = sigmaVal
	e.lastUpdated = updated
}
