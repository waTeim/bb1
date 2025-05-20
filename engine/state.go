package engine

import (
	"context"
	"math"
	"sync"
	"time"

	slog "log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/wat.im/bb1/oracle"
)

// Tick represents a price sample at a given timestamp.
type Tick struct {
	Prices oracle.PriceData
	TS     int64 // epoch milliseconds
}

// Engine processes ticks into an EMA-based fair ratio with noise clamping
// and rolling sigma over a fixed-size window.
// ProcessTick is thread-safe.
type Engine struct {
	alpha float64

	mu    sync.RWMutex
	ema   float64
	sigma float64

	buffer []float64
	tsBuf  []int64
	pos    int
	count  int
	// rolling sums for mean & variance
	sum   float64
	sumsq float64

	lastUpdated time.Time

	// Prometheus metrics
	fairGauge  prometheus.Gauge
	spotGauge  prometheus.Gauge
	sigmaGauge prometheus.Gauge
}

const (
	// maximum number of ticks to retain for sigma calculation
	ringSize = 6888
	// failover downtime threshold
	downThreshold = 120 * time.Second
)

// New creates and registers a new Engine with EMA window N for alpha.
func New(window int) *Engine {
	alpha := 2.0 / (float64(window) + 1)

	fair := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "fair_ratio",
		Help: "EMA-based fair price ratio",
	})
	spot := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "spot_ratio",
		Help: "Last spot price ratio",
	})
	sig := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "ratio_sigma",
		Help: "Rolling standard deviation of spot price",
	})
	prometheus.MustRegister(fair, spot, sig)

	return &Engine{
		alpha:      alpha,
		buffer:     make([]float64, ringSize),
		tsBuf:      make([]int64, ringSize),
		fairGauge:  fair,
		spotGauge:  spot,
		sigmaGauge: sig,
	}
}

// ProcessTick ingests a tick: updates ring buffer, computes sigma, clamps delta, updates EMA.
func (e *Engine) ProcessTick(t Tick) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// extract the spot price
	price := t.Prices.Spot

	// --- ring buffer update (unchanged) ---
	var old float64
	if e.count < len(e.buffer) {
		e.buffer[e.pos] = price
		e.tsBuf[e.pos] = t.TS
		e.sum += price
		e.sumsq += price * price
		e.count++
	} else {
		old = e.buffer[e.pos]
		e.buffer[e.pos] = price
		e.tsBuf[e.pos] = t.TS
		e.sum += price - old
		e.sumsq += price*price - old*old
	}
	e.pos = (e.pos + 1) % len(e.buffer)

	// --- compute rolling sigma (unchanged) ---
	mean := e.sum / float64(e.count)
	var variance float64
	if e.count > 1 {
		variance = e.sumsq/float64(e.count) - mean*mean
		if variance < 0 {
			variance = 0
		}
		e.sigma = math.Sqrt(variance)
	} else {
		e.sigma = 0
	}

	// --- EMA update: seed on first tick ---
	if e.count == 1 {
		// First data point: initialize EMA exactly to the spot price
		e.ema = price
	} else {
		// standard EMA step with ±3σ clamping
		delta := price - e.ema
		thr := 3 * e.sigma
		if delta > thr {
			delta = thr
		} else if delta < -thr {
			delta = -thr
		}
		e.ema += e.alpha * delta
	}

	e.lastUpdated = time.UnixMilli(t.TS)

	// --- update Prometheus metrics ---
	e.fairGauge.Set(e.ema)
	e.spotGauge.Set(price)
	e.sigmaGauge.Set(e.sigma)

	slog.Debug("engine tick",
		"ema", e.ema,
		"sigma", e.sigma,
		"spot", price,
	)
}

// Run consumes ticks from tickCh until ctx.Done() and processes them.
func Run(ctx context.Context, tickCh <-chan Tick, eng *Engine) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-tickCh:
			eng.ProcessTick(t)
		}
	}
}

// StartPoller begins polling prices using an aggregator at interval and sends ticks.
func StartPoller(ctx context.Context, agg *oracle.Aggregator, tickCh chan<- Tick, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prices, err := agg.FetchAll(ctx)
			if err != nil {
				slog.Warn("poller: all sources failed", "error", err)
				continue
			}
			now := time.Now().UnixMilli()
			slog.Info("poller: publishing aggregated tick", "spot", prices.Spot, "usd", prices.USD, "ts", now)
			tickCh <- Tick{Prices: prices, TS: now}
		}
	}
}
