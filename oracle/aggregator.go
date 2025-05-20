// Package oracle provides multi-source price aggregation with backoff and reliability
package oracle

import (
	"context"
	"fmt"
	"sync"
	"time"

	slog "log/slog"
)

// Source represents an upstream price provider.
type Source int

const (
	SourceJupiter Source = iota
	SourceCoingecko
	SourceDexScreener
)

// String returns the name of the source.
func (s Source) String() string {
	switch s {
	case SourceJupiter:
		return "Jupiter"
	case SourceCoingecko:
		return "Coingecko"
	case SourceDexScreener:
		return "DexScreener"
	default:
		return "Unknown"
	}
}

// Config holds aggregator-specific settings.
type AggregatorConfig struct {
	Primary           Source        // primary source when reliability is tied
	ReliabilityWindow time.Duration // duration to consider for reliability ranking
	BackoffInitial    time.Duration // initial backoff on error
	BackoffMax        time.Duration // maximum backoff
}

// status tracks per-source state.
type status struct {
	lastSuccess time.Time
	lastError   time.Time
	backoff     time.Duration
	nextAttempt time.Time
}

// Aggregator fetches from all sources, applies backoff, and averages prices.
// Not safe for concurrent use.
type Aggregator struct {
	pc     *PriceClient
	cfg    AggregatorConfig
	status map[Source]*status
	mu     sync.Mutex
}

// NewAggregator creates an Aggregator with provided config.
func NewAggregator(pc *PriceClient, cfg AggregatorConfig) *Aggregator {
	st := map[Source]*status{
		SourceJupiter:     {backoff: cfg.BackoffInitial},
		SourceCoingecko:   {backoff: cfg.BackoffInitial},
		SourceDexScreener: {backoff: cfg.BackoffInitial},
	}
	return &Aggregator{pc: pc, cfg: cfg, status: st}
}

// FetchAll fetches prices from all sources, honoring backoff, and returns the mean spot price.
func (ag *Aggregator) FetchAll(ctx context.Context) (PriceData, error) {
	now := time.Now()
	ag.mu.Lock()
	defer ag.mu.Unlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	var hitsSpot []float64
	var hitsUSD []float64
	var errs []error

	for src, st := range ag.status {
		// check backoff
		if now.Before(st.nextAttempt) {
			slog.Info("skipping source due to backoff", "source", src, "nextAttempt", st.nextAttempt)
			continue
		}

		wg.Add(1)
		go func(src Source, st *status) {
			defer wg.Done()
			var pd PriceData
			var err error
			switch src {
			case SourceJupiter:
				pd, err = ag.pc.FetchJupiter(ctx)
			case SourceCoingecko:
				pd, err = ag.pc.FetchCoingecko(ctx)
			case SourceDexScreener:
				pd, err = ag.pc.FetchDexScreener(ctx)
			}

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				// record error and apply backoff
				st.lastError = time.Now()
				st.backoff *= 2
				if st.backoff > ag.cfg.BackoffMax {
					st.backoff = ag.cfg.BackoffMax
				}
				st.nextAttempt = time.Now().Add(st.backoff)
				slog.Warn("source fetch error, backing off", "source", src, "error", err, "backoff", st.backoff)
				errs = append(errs, fmt.Errorf("%s: %w", src, err))
			} else {
				// Skip zero spot (no data)
				if pd.Spot == 0.0 {
					slog.Warn("source returned zero spot price, skipping", "source", src)
				} else {
					// accept spot
					st.lastSuccess = time.Now()
					st.backoff = ag.cfg.BackoffInitial
					hitsSpot = append(hitsSpot, pd.Spot)
				}
				// Skip zero USD (might not always be provided)
				if pd.USD == 0.0 {
					slog.Warn("source returned zero USD price, skipping USD", "source", src)
				} else {
					hitsUSD = append(hitsUSD, pd.USD)
				}
				if pd.Spot != 0.0 {
					slog.Info("source fetch success", "source", src, "spot", pd.Spot, "usd", pd.USD)
				}
			}
		}(src, st)
	}

	wg.Wait()

	if len(hitsSpot) == 0 {
		return PriceData{}, fmt.Errorf("all sources failed or returned zero spot: %v", errs)
	}

	// average spot
	sumSpot := 0.0
	for _, v := range hitsSpot {
		sumSpot += v
	}
	meanSpot := sumSpot / float64(len(hitsSpot))

	// average USD if any
	meanUSD := 0.0
	if len(hitsUSD) > 0 {
		sumUSD := 0.0
		for _, v := range hitsUSD {
			sumUSD += v
		}
		meanUSD = sumUSD / float64(len(hitsUSD))
	}

	return PriceData{Spot: meanSpot, USD: meanUSD}, nil
}
