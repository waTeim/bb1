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
// FetchAll fetches prices from all sources, honors backoff, and returns the mean BILLY→SOL, BILLY→USD, and SOL→USD prices.
func (ag *Aggregator) FetchAll(ctx context.Context) (PriceData, error) {
	now := time.Now()
	ag.mu.Lock()
	defer ag.mu.Unlock()

	var (
		mu           sync.Mutex
		wg           sync.WaitGroup
		hitsBillySol []float64
		hitsBillyUSD []float64
		hitsSolUSD   []float64
		errs         []error
	)

	for src, st := range ag.status {
		if now.Before(st.nextAttempt) {
			slog.Info("skipping source due to backoff", "source", src, "nextAttempt", st.nextAttempt)
			continue
		}

		wg.Add(1)
		go func(src Source, st *status) {
			defer wg.Done()

			pd, err := func() (PriceData, error) {
				switch src {
				case SourceJupiter:
					return ag.pc.FetchJupiter(ctx)
				case SourceCoingecko:
					return ag.pc.FetchCoingecko(ctx)
				case SourceDexScreener:
					return ag.pc.FetchDexScreener(ctx)
				}
				return PriceData{}, nil
			}()

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				// apply backoff
				st.lastError = time.Now()
				st.backoff *= 2
				if st.backoff > ag.cfg.BackoffMax {
					st.backoff = ag.cfg.BackoffMax
				}
				st.nextAttempt = time.Now().Add(st.backoff)
				slog.Warn("source fetch error, backing off", "source", src, "error", err, "backoff", st.backoff)
				errs = append(errs, fmt.Errorf("%s: %w", src, err))
				return
			}

			// BILLY→SOL
			if pd.BillySol == 0 {
				slog.Warn("source returned zero BILLY→SOL price, skipping", "source", src)
			} else {
				st.lastSuccess = time.Now()
				st.backoff = ag.cfg.BackoffInitial
				hitsBillySol = append(hitsBillySol, pd.BillySol)
			}

			// BILLY→USD
			if pd.BillyUSD == 0 {
				slog.Warn("source returned zero BILLY→USD price, skipping", "source", src)
			} else {
				hitsBillyUSD = append(hitsBillyUSD, pd.BillyUSD)
			}

			// SOL→USD
			if pd.SolUSD == 0 {
				slog.Warn("source returned zero SOL→USD price, skipping", "source", src)
			} else {
				hitsSolUSD = append(hitsSolUSD, pd.SolUSD)
			}

			slog.Info("source fetch success",
				"source", src,
				"billySol", pd.BillySol,
				"billyUSD", pd.BillyUSD,
				"solUSD", pd.SolUSD,
			)
		}(src, st)
	}

	wg.Wait()

	// BILLY→SOL is required
	if len(hitsBillySol) == 0 {
		return PriceData{}, fmt.Errorf("all sources failed or returned zero BILLY→SOL: %v", errs)
	}
	// BILLY→USD is required
	if len(hitsBillyUSD) == 0 {
		return PriceData{}, fmt.Errorf("all sources failed or returned zero BILLY→USD: %v", errs)
	}
	// SOL→USD is required
	if len(hitsSolUSD) == 0 {
		return PriceData{}, fmt.Errorf("all sources failed or returned zero SOL→USD: %v", errs)
	}

	// average BILLY→SOL
	sum := 0.0
	for _, v := range hitsBillySol {
		sum += v
	}
	meanBillySol := sum / float64(len(hitsBillySol))

	// average BILLY→USD
	sum = 0.0
	for _, v := range hitsBillyUSD {
		sum += v
	}
	meanBillyUSD := sum / float64(len(hitsBillyUSD))

	// average SOL→USD
	sum = 0.0
	for _, v := range hitsSolUSD {
		sum += v
	}
	meanSolUSD := sum / float64(len(hitsSolUSD))

	return PriceData{
		BillySol: meanBillySol,
		BillyUSD: meanBillyUSD,
		SolUSD:   meanSolUSD,
	}, nil

}
