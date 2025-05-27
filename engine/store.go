package engine

import (
	"time"

	"github.com/wat.im/bb1/oracle"
)

// Snapshot extracts the current engine state: EMAs and σ’s for each price leg,
// tick buffer, and last update time.
// Returned slice of Tick is in chronological order (oldest first).
func (e *Engine) Snapshot() (
	emaBillySol, emaBillyUSD, emaSolUSD float64,
	sigmaBillySol, sigmaBillyUSD, sigmaSolUSD float64,
	ticks []Tick,
	updated time.Time,
) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	// Current EMAs & σ’s
	emaBillySol = e.emaBillySol
	emaBillyUSD = e.emaBillyUSD
	emaSolUSD = e.emaSolUSD
	sigmaBillySol = e.sigmaBillySol
	sigmaBillyUSD = e.sigmaBillyUSD
	sigmaSolUSD = e.sigmaSolUSD
	updated = e.lastUpdated

	// Collect ticks in chronological order
	n := e.count
	ticks = make([]Tick, n)
	window := len(e.bufferBillySol) // same for all buffers
	for i := 0; i < n; i++ {
		idx := (e.pos - n + i + window) % window
		ticks[i] = Tick{
			Prices: oracle.PriceData{
				BillySol: e.bufferBillySol[idx],
				BillyUSD: e.bufferBillyUSD[idx],
				SolUSD:   e.bufferSolUSD[idx],
			},
			TS: e.tsBuf[idx],
		}
	}
	return
}

// Restore replaces the engine's state with a snapshot: tick buffer, EMAs, σ’s, and last update time.
func (e *Engine) Restore(
	saved []Tick,
	emaBillySol, emaBillyUSD, emaSolUSD float64,
	sigmaBillySol, sigmaBillyUSD, sigmaSolUSD float64,
	updated time.Time,
) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Determine window size from buffer length
	window := len(e.bufferBillySol)

	// Trim saved ticks to at most 'window' most recent entries
	n := len(saved)
	if n > window {
		saved = saved[n-window:]
		n = window
	}

	// Update count and position
	e.count = n
	e.pos = n % window

	// Reset rolling sums for all three series
	e.sumBillySol, e.sumsqBillySol = 0, 0
	e.sumBillyUSD, e.sumsqBillyUSD = 0, 0
	e.sumSolUSD, e.sumsqSolUSD = 0, 0

	// Rebuild buffers, sums, and tsBuf
	for i := 0; i < n; i++ {
		tick := saved[i]
		bs := tick.Prices.BillySol
		bu := tick.Prices.BillyUSD
		su := tick.Prices.SolUSD

		e.bufferBillySol[i] = bs
		e.bufferBillyUSD[i] = bu
		e.bufferSolUSD[i] = su
		e.tsBuf[i] = tick.TS

		e.sumBillySol += bs
		e.sumsqBillySol += bs * bs

		e.sumBillyUSD += bu
		e.sumsqBillyUSD += bu * bu

		e.sumSolUSD += su
		e.sumsqSolUSD += su * su
	}

	// Restore EMA and σ values
	e.emaBillySol = emaBillySol
	e.emaBillyUSD = emaBillyUSD
	e.emaSolUSD = emaSolUSD
	e.sigmaBillySol = sigmaBillySol
	e.sigmaBillyUSD = sigmaBillyUSD
	e.sigmaSolUSD = sigmaSolUSD

	e.lastUpdated = updated
}
