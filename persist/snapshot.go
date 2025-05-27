package persist

import (
	"context"
	"fmt"
	"strconv"
	"time"

	slog "log/slog"

	"github.com/wat.im/bb1/engine"

	"github.com/redis/go-redis/v9"
	"github.com/vmihailenco/msgpack"
)

// Store handles persistence of engine snapshots in Redis under a given key prefix.
type Store struct {
	rdb    *redis.Client
	prefix string
}

// New creates a new Store using the provided Redis client and key prefix.
func New(rdb *redis.Client, prefix string) *Store {
	return &Store{rdb: rdb, prefix: prefix}
}

// key constructs the Redis key for a specific field (ema, sigma, updated, ring).
func (s *Store) key(field string) string {
	return fmt.Sprintf("%s:%s", s.prefix, field)
}

// Snapshot represents the persisted state of the engine.
// On load you can replay the Ticks slice to rebuild buffer, sum, sumsq, count, pos.
// Now includes three series: BILLY→SOL, BILLY→USD, and SOL→USD
// with their EMAs and sigmas.
type Snapshot struct {
	EMABillySol   float64       // last persisted EMA for BILLY→SOL price
	EMABillyUSD   float64       // last persisted EMA for BILLY→USD price
	EMASolUSD     float64       // last persisted EMA for SOL→USD price
	SigmaBillySol float64       // last persisted rolling σ for BILLY→SOL price
	SigmaBillyUSD float64       // last persisted rolling σ for BILLY→USD price
	SigmaSolUSD   float64       // last persisted rolling σ for SOL→USD price
	Ticks         []engine.Tick // full tick history (each Tick contains BillySol, BillyUSD, SolUSD)
	Updated       time.Time     // timestamp of last update
}

// Load retrieves the engine snapshot from Redis. Returns an error if any field is missing or invalid.
func (s *Store) Load(ctx context.Context) (*Snapshot, error) {
	// Retrieve all fields in one MGet
	vals, err := s.rdb.MGet(ctx,
		s.key("ema_billy_sol"),
		s.key("ema_billy_usd"),
		s.key("ema_sol_usd"),
		s.key("sigma_billy_sol"),
		s.key("sigma_billy_usd"),
		s.key("sigma_sol_usd"),
		s.key("updated"),
		s.key("ring"),
	).Result()
	if err != nil {
		return nil, err
	}
	if len(vals) != 8 {
		return nil, fmt.Errorf("snapshot missing fields: expected 8, got %d", len(vals))
	}
	for i, v := range vals {
		if v == nil {
			return nil, fmt.Errorf("snapshot field %d is nil", i)
		}
	}

	// Parse EMAs
	emaBillySol, err := strconv.ParseFloat(vals[0].(string), 64)
	if err != nil {
		return nil, fmt.Errorf("invalid ema_billy_sol: %w", err)
	}
	emaBillyUSD, err := strconv.ParseFloat(vals[1].(string), 64)
	if err != nil {
		return nil, fmt.Errorf("invalid ema_billy_usd: %w", err)
	}
	emaSolUSD, err := strconv.ParseFloat(vals[2].(string), 64)
	if err != nil {
		return nil, fmt.Errorf("invalid ema_sol_usd: %w", err)
	}

	// Parse sigmas
	sigmaBillySol, err := strconv.ParseFloat(vals[3].(string), 64)
	if err != nil {
		return nil, fmt.Errorf("invalid sigma_billy_sol: %w", err)
	}
	sigmaBillyUSD, err := strconv.ParseFloat(vals[4].(string), 64)
	if err != nil {
		return nil, fmt.Errorf("invalid sigma_billy_usd: %w", err)
	}
	sigmaSolUSD, err := strconv.ParseFloat(vals[5].(string), 64)
	if err != nil {
		return nil, fmt.Errorf("invalid sigma_sol_usd: %w", err)
	}

	// Parse Updated timestamp (epoch ms)
	updatedMs, err := strconv.ParseInt(vals[6].(string), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid updated timestamp: %w", err)
	}

	// Unmarshal ring buffer ticks
	data, ok := vals[7].([]byte)
	if !ok {
		return nil, fmt.Errorf("invalid ring data type: %T", vals[7])
	}
	var ticks []engine.Tick
	if err := msgpack.Unmarshal(data, &ticks); err != nil {
		return nil, fmt.Errorf("unmarshal ring ticks: %w", err)
	}

	return &Snapshot{
		EMABillySol:   emaBillySol,
		EMABillyUSD:   emaBillyUSD,
		EMASolUSD:     emaSolUSD,
		SigmaBillySol: sigmaBillySol,
		SigmaBillyUSD: sigmaBillyUSD,
		SigmaSolUSD:   sigmaSolUSD,
		Ticks:         ticks,
		Updated:       time.UnixMilli(updatedMs),
	}, nil
}

// Save writes the current engine snapshot to Redis atomically using a pipeline.
func (s *Store) Save(ctx context.Context, eng *engine.Engine) error {
	// Retrieve triple‐series snapshot: EMAs and σ’s for BILLY→SOL, BILLY→USD, SOL→USD, tick history, and timestamp
	emaBillySol, emaBillyUSD, emaSolUSD,
		sigmaBillySol, sigmaBillyUSD, sigmaSolUSD,
		ticks, updated := eng.Snapshot()

	// Marshal tick history as msgpack
	data, err := msgpack.Marshal(ticks)
	if err != nil {
		return err
	}

	// Atomically set all fields
	_, err = s.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		// EMAs
		pipe.Set(ctx, s.key("ema_billy_sol"), fmt.Sprintf("%f", emaBillySol), 0)
		pipe.Set(ctx, s.key("ema_billy_usd"), fmt.Sprintf("%f", emaBillyUSD), 0)
		pipe.Set(ctx, s.key("ema_sol_usd"), fmt.Sprintf("%f", emaSolUSD), 0)
		// Sigmas
		pipe.Set(ctx, s.key("sigma_billy_sol"), fmt.Sprintf("%f", sigmaBillySol), 0)
		pipe.Set(ctx, s.key("sigma_billy_usd"), fmt.Sprintf("%f", sigmaBillyUSD), 0)
		pipe.Set(ctx, s.key("sigma_sol_usd"), fmt.Sprintf("%f", sigmaSolUSD), 0)
		// Updated timestamp
		pipe.Set(ctx, s.key("updated"), fmt.Sprintf("%d", updated.UnixMilli()), 0)
		// Tick ring
		pipe.Set(ctx, s.key("ring"), data, 0)

		slog.Info("snapshot saved",
			"ema_billy_sol", emaBillySol,
			"ema_billy_usd", emaBillyUSD,
			"ema_sol_usd", emaSolUSD,
			"sigma_billy_sol", sigmaBillySol,
			"sigma_billy_usd", sigmaBillyUSD,
			"sigma_sol_usd", sigmaSolUSD,
			"ticks", len(ticks),
			"updated", updated,
		)
		return nil
	})
	if err != nil {
		slog.Error("persist: snapshot save failed", "error", err)
	}
	return err
}

// Start begins a background loop that saves the engine snapshot every interval until ctx is done.
func Start(ctx context.Context, store *Store, eng *engine.Engine, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := store.Save(ctx, eng); err != nil {
				slog.Error("persist: error saving snapshot", "error", err)
			}
		}
	}
}
