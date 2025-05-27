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

// Engine processes ticks into three EMAs (BILLY→SOL, BILLY→USD, SOL→USD)
// with ±3σ noise‐clamping and rolling σ over a fixed‐size window.
// ProcessTick is thread‐safe.
type Engine struct {
	alpha float64 // steady‐state EMA smoothing factor (2/(N+1))

	mu sync.RWMutex

	// current Spot
	billySol float64
	billyUSD float64
	solUSD   float64

	// current EMAs
	emaBillySol float64
	emaBillyUSD float64
	emaSolUSD   float64

	// rolling‐window state (shared size/time buffer)
	bufferBillySol []float64
	bufferBillyUSD []float64
	bufferSolUSD   []float64
	tsBuf          []int64
	pos            int
	count          int

	// rolling sums for mean & variance
	sumBillySol, sumsqBillySol float64
	sumBillyUSD, sumsqBillyUSD float64
	sumSolUSD, sumsqSolUSD     float64

	// rolling σ for each series
	sigmaBillySol float64
	sigmaBillyUSD float64
	sigmaSolUSD   float64

	lastUpdated time.Time

	// Prometheus metrics
	spotBillySolGauge prometheus.Gauge
	spotBillyUSDGauge prometheus.Gauge
	spotSolUSDGauge   prometheus.Gauge

	emaBillySolGauge prometheus.Gauge
	emaBillyUSDGauge prometheus.Gauge
	emaSolUSDGauge   prometheus.Gauge

	sigmaBillySolGauge prometheus.Gauge
	sigmaBillyUSDGauge prometheus.Gauge
	sigmaSolUSDGauge   prometheus.Gauge
}

const (
	// maximum number of ticks to retain for sigma calculation
	ringSize = 6888
	// failover downtime threshold
	downThreshold = 120 * time.Second
)

// New creates and registers a new Engine with a rolling window of size `window`
// and an N-tick EMA (α = 2/(window+1)) for each of the three price legs:
//   - BILLY→SOL, BILLY→USD, and SOL→USD.
func New(window int) *Engine {
	// 1) Compute steady‐state α = 2/(N+1)
	alpha := 2.0 / (float64(window) + 1.0)

	// 2) Create gauges for BILLY→SOL
	spotBillySolG := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bb1_spot_billy_sol_price",
		Help: "Last observed price of 1 BILLY in SOL",
	})
	emaBillySolG := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bb1_ema_billy_sol_price",
		Help: "EMA of BILLY→SOL price",
	})
	sigmaBillySolG := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bb1_sigma_billy_sol_price",
		Help: "Rolling standard deviation of BILLY→SOL price",
	})

	// 3) Create gauges for BILLY→USD
	spotBillyUSDG := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bb1_spot_billy_usd_price",
		Help: "Last observed price of 1 BILLY in USD",
	})
	emaBillyUSDG := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bb1_ema_billy_usd_price",
		Help: "EMA of BILLY→USD price",
	})
	sigmaBillyUSDG := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bb1_sigma_billy_usd_price",
		Help: "Rolling standard deviation of BILLY→USD price",
	})

	// 4) Create gauges for SOL→USD
	spotSolUSDG := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bb1_spot_sol_usd_price",
		Help: "Last observed price of 1 SOL in USD",
	})
	emaSolUSDG := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bb1_ema_sol_usd_price",
		Help: "EMA of SOL→USD price",
	})
	sigmaSolUSDG := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "bb1_sigma_sol_usd_price",
		Help: "Rolling standard deviation of SOL→USD price",
	})

	// 5) Register all nine gauges
	prometheus.MustRegister(
		spotBillySolG, emaBillySolG, sigmaBillySolG,
		spotBillyUSDG, emaBillyUSDG, sigmaBillyUSDG,
		spotSolUSDG, emaSolUSDG, sigmaSolUSDG,
	)

	// 6) Construct the Engine with empty buffers of length `window`
	return &Engine{
		alpha:          alpha,
		bufferBillySol: make([]float64, window),
		bufferBillyUSD: make([]float64, window),
		bufferSolUSD:   make([]float64, window),
		tsBuf:          make([]int64, window),
		// Prometheus metrics
		spotBillySolGauge:  spotBillySolG,
		emaBillySolGauge:   emaBillySolG,
		sigmaBillySolGauge: sigmaBillySolG,
		spotBillyUSDGauge:  spotBillyUSDG,
		emaBillyUSDGauge:   emaBillyUSDG,
		sigmaBillyUSDGauge: sigmaBillyUSDG,
		spotSolUSDGauge:    spotSolUSDG,
		emaSolUSDGauge:     emaSolUSDG,
		sigmaSolUSDGauge:   sigmaSolUSDG,
	}
}

// updateBuffer rolls your ring buffer & maintains sum/sumsq for one series (SOL or USD).
// - price: the new tick price for this series
// - buf:   the slice backing that series’ ring buffer (e.bufferSOL or e.bufferUSD)
// - sum:   pointer to that series’ rolling sum (e.sumSOL or e.sumUSD)
// - sumsq: pointer to that series’ rolling sum of squares (e.sumsqSOL or e.sumsqUSD)
// - ts:    the timestamp for this tick
func (e *Engine) updateBuffer(
	price float64,
	buf []float64,
	sum *float64,
	sumsq *float64,
	ts int64,
) {
	var old float64

	// still filling up
	if e.count < len(buf) {
		*sum += price
		*sumsq += price * price
		buf[e.pos] = price
		e.tsBuf[e.pos] = ts
		e.count++
	} else {
		// buffer full: remove oldest, add new
		old = buf[e.pos]
		*sum += price - old
		*sumsq += price*price - old*old
		buf[e.pos] = price
		e.tsBuf[e.pos] = ts
	}

	// advance ring position
	e.pos = (e.pos + 1) % len(buf)
}

// computeSigma returns the rolling standard deviation given the rolling sum and sum of squares.
// It uses e.count as the current number of samples.
func (e *Engine) computeSigma(sum, sumsq float64) float64 {
	if e.count < 2 {
		return 0
	}
	mean := sum / float64(e.count)
	// variance = E[x²] − (E[x])²
	v := sumsq/float64(e.count) - mean*mean
	if v < 0 {
		v = 0
	}
	return math.Sqrt(v)
}

// updateEMA applies the dynamic-α warmup and ±3σ clamp.
// `target` should point at one of e.emaBillySol, e.emaBillyUSD, or e.emaSolUSD.
func (e *Engine) updateEMA(target *float64, price, sigma float64) {
	// 1) Seed on the very first tick
	if e.count == 1 {
		*target = price
		return
	}

	// 2) Determine the effective α:
	//    - while warming up (count < window), α = 2/(count+1)
	//    - afterwards, α = e.alpha (which is 2/(window+1))
	period := len(e.bufferBillySol) // all three buffers share the same window size
	var alphaEff float64
	if e.count < period {
		alphaEff = 2.0 / (float64(e.count) + 1.0)
	} else {
		alphaEff = e.alpha
	}

	// 3) Compute the innovation, clamped to ±3σ
	delta := price - *target
	thr := 3 * sigma
	if delta > thr {
		delta = thr
	} else if delta < -thr {
		delta = -thr
	}

	// 4) Update the EMA
	*target += alphaEff * delta
}

// updateMetrics pushes all nine Prometheus gauges for the three price legs:
//
//	– BILLY→SOL: spot, EMA, σ
//	– BILLY→USD: spot, EMA, σ
//	– SOL→USD:   spot, EMA, σ
func (e *Engine) updateMetrics() {
	// BILLY→SOL series
	e.spotBillySolGauge.Set(e.billySol)
	e.emaBillySolGauge.Set(e.emaBillySol)
	e.sigmaBillySolGauge.Set(e.sigmaBillySol)

	// BILLY→USD series
	e.spotBillyUSDGauge.Set(e.billyUSD)
	e.emaBillyUSDGauge.Set(e.emaBillyUSD)
	e.sigmaBillyUSDGauge.Set(e.sigmaBillyUSD)

	// SOL→USD series
	e.spotSolUSDGauge.Set(e.solUSD)
	e.emaSolUSDGauge.Set(e.emaSolUSD)
	e.sigmaSolUSDGauge.Set(e.sigmaSolUSD)
}

// ProcessTick ingests a tick: updates three buffers (BILLY→SOL, BILLY→USD, SOL→USD),
// computes each σ, applies the EMA update with ±3σ clamp, and pushes all 9 metrics.
func (e *Engine) ProcessTick(t Tick) {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 1) Extract the three price legs + timestamp
	e.billySol = t.Prices.BillySol
	e.billyUSD = t.Prices.BillyUSD
	e.solUSD = t.Prices.SolUSD
	ts := t.TS

	// 2) Update rolling buffers for each leg
	e.updateBuffer(e.billySol, e.bufferBillySol, &e.sumBillySol, &e.sumsqBillySol, ts)
	e.updateBuffer(e.billyUSD, e.bufferBillyUSD, &e.sumBillyUSD, &e.sumsqBillyUSD, ts)
	e.updateBuffer(e.solUSD, e.bufferSolUSD, &e.sumSolUSD, &e.sumsqSolUSD, ts)

	// 3) Compute rolling sigmas
	e.sigmaBillySol = e.computeSigma(e.sumBillySol, e.sumsqBillySol)
	e.sigmaBillyUSD = e.computeSigma(e.sumBillyUSD, e.sumsqBillyUSD)
	e.sigmaSolUSD = e.computeSigma(e.sumSolUSD, e.sumsqSolUSD)

	// 4) Update EMAs (with dynamic-α warmup and ±3σ clamp)
	e.updateEMA(&e.emaBillySol, e.billySol, e.sigmaBillySol)
	e.updateEMA(&e.emaBillyUSD, e.billyUSD, e.sigmaBillyUSD)
	e.updateEMA(&e.emaSolUSD, e.solUSD, e.sigmaSolUSD)

	// 5) Record last update time
	e.lastUpdated = time.UnixMilli(ts)

	// 6) Push all metrics (3 spot + 3 EMA + 3 σ)
	e.updateMetrics()

	// 7) Debug log
	slog.Debug("engine tick",
		"ema_billySol", e.emaBillySol,
		"ema_billyUSD", e.emaBillyUSD,
		"ema_solUSD", e.emaSolUSD,
		"sigma_billySol", e.sigmaBillySol,
		"sigma_billyUSD", e.sigmaBillyUSD,
		"sigma_solUSD", e.sigmaSolUSD,
		"spot_billySol", e.billySol,
		"spot_billyUSD", e.billyUSD,
		"spot_solUSD", e.solUSD,
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
			pd, err := agg.FetchAll(ctx)
			if err != nil {
				slog.Warn("poller: all sources failed", "error", err)
				continue
			}
			now := time.Now().UnixMilli()
			slog.Info("poller: publishing aggregated tick",
				"billySol", pd.BillySol,
				"billyUSD", pd.BillyUSD,
				"solUSD", pd.SolUSD,
				"ts", now,
			)
			tickCh <- Tick{Prices: pd, TS: now}
		}
	}
}
