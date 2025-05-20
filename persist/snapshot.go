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
type Snapshot struct {
	EMA     float64       // persisted EMA value
	Sigma   float64       // persisted sigma value
	Ticks   []engine.Tick // persisted tick history
	Updated time.Time     // timestamp of last update
}

// Load retrieves the engine snapshot from Redis. Returns an error if any field is missing or invalid.
func (s *Store) Load(ctx context.Context) (*Snapshot, error) {
	// Retrieve all fields in one MGet
	vals, err := s.rdb.MGet(ctx,
		s.key("ema"),
		s.key("sigma"),
		s.key("updated"),
		s.key("ring"),
	).Result()
	if err != nil {
		return nil, err
	}
	if len(vals) != 4 || vals[0] == nil || vals[1] == nil || vals[2] == nil || vals[3] == nil {
		return nil, fmt.Errorf("snapshot missing fields")
	}

	// Parse EMA and Sigma
	ema, err := strconv.ParseFloat(vals[0].(string), 64)
	if err != nil {
		return nil, err
	}
	sigma, err := strconv.ParseFloat(vals[1].(string), 64)
	if err != nil {
		return nil, err
	}

	// Parse Updated timestamp (epoch ms)
	updatedMs, err := strconv.ParseInt(vals[2].(string), 10, 64)
	if err != nil {
		return nil, err
	}

	// Unmarshal ring buffer
	data, ok := vals[3].([]byte)
	if !ok {
		return nil, fmt.Errorf("invalid ring data type: %T", vals[3])
	}
	var ticks []engine.Tick
	if err := msgpack.Unmarshal(data, &ticks); err != nil {
		return nil, err
	}

	return &Snapshot{
		EMA:     ema,
		Sigma:   sigma,
		Ticks:   ticks,
		Updated: time.UnixMilli(updatedMs),
	}, nil
}

// Save writes the current engine snapshot to Redis atomically using a pipeline.
func (s *Store) Save(ctx context.Context, eng *engine.Engine) error {
	ema, sigma, ticks, updated := eng.Snapshot()
	// Marshal tick history
	data, err := msgpack.Marshal(ticks)
	if err != nil {
		return err
	}

	// Atomically set all fields
	_, err = s.rdb.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
		pipe.Set(ctx, s.key("ema"), fmt.Sprintf("%f", ema), 0)
		pipe.Set(ctx, s.key("sigma"), fmt.Sprintf("%f", sigma), 0)
		pipe.Set(ctx, s.key("updated"), updated.UnixMilli(), 0)
		pipe.Set(ctx, s.key("ring"), data, 0)
		slog.Info("snapshot saved", "ema", ema, "sigma", sigma, "ticks", len(ticks), "updated", updated)
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
