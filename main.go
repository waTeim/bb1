package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	slog "log/slog"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	echoSwagger "github.com/swaggo/echo-swagger"

	_ "github.com/wat.im/bb1/docs"
	"github.com/wat.im/bb1/engine"
	"github.com/wat.im/bb1/oracle"
	"github.com/wat.im/bb1/persist"
)

// Config holds service and aggregator configuration loaded from JSON file.
type Config struct {
	Port                 int    `json:"port"`
	PollMs               int    `json:"poll_ms"`
	BackupPollMs1        int    `json:"backup_poll_ms1"`
	BackupPollMs2        int    `json:"backup_poll_ms2"`
	EmaWindow            int    `json:"ema_window"`
	RedisURL             string `json:"redis_url"`
	SnapshotSec          int    `json:"snapshot_sec"`
	LogLevel             string `json:"log_level"`
	AggregatorPrimary    string `json:"aggregator_primary"`
	ReliabilityWindowSec int    `json:"reliability_window_sec"`
	BackoffInitialMs     int    `json:"backoff_initial_ms"`
	BackoffMaxMs         int    `json:"backoff_max_ms"`
	CoingeckoAPIKey      string `json:"coingecko_api_key"`
	CoingeckoKeyType     string `json:"coingecko_key_type"`
}

// RatioResponse defines the JSON response for /ratio.
type RatioResponse struct {
	Fair    float64 `json:"fair"`
	Spot    float64 `json:"spot"`
	Sigma   float64 `json:"sigma"`
	Updated string  `json:"updated"`
}

// Tick represents a price sample at a given time.
//type Tick struct {
//	Price float64
//	TS    int64 // epoch milliseconds
//}

var eInst *engine.Engine
var logLevel = slog.LevelVar{}

// loadConfig reads JSON config from /etc/bb1.conf.
func loadConfig(path string) (*Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening config file: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	cfg := &Config{}
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("decoding config JSON: %w", err)
	}
	return cfg, nil
}

// initLogger configures slog structured logger with the configured level.
func initLogger(level string) {
	// Parse log level
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	logLevel.Set(lvl)

	// Create JSON handler with level filter
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: &logLevel})
	logger := slog.New(handler)
	slog.SetDefault(logger)
}

// initRedis initializes and verifies the Redis client.
func initRedis(url string) *redis.Client {
	opts, err := redis.ParseURL(url)
	if err != nil {
		slog.Error("invalid Redis URL", "error", err)
		os.Exit(1)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		slog.Error("cannot connect to Redis", "error", err)
		os.Exit(1)
	}
	return client
}

// startPipeline initializes engine, aggregator, poller, and snapshotter.
func startPipeline(ctx context.Context, cfg *Config, rdb *redis.Client) {
	// Persistence store
	store := persist.New(rdb, "BillyR")

	// Engine
	e := engine.New(cfg.EmaWindow)
	eInst = e

	// Attempt to restore a recent snapshot (<24h old)
	if snap, err := store.Load(ctx); err == nil {
		if time.Since(snap.Updated) < 24*time.Hour {
			e.Restore(
				snap.Ticks,
				// EMAs
				snap.EMABillySol, snap.EMABillyUSD, snap.EMASolUSD,
				// Sigmas
				snap.SigmaBillySol, snap.SigmaBillyUSD, snap.SigmaSolUSD,
				// last update timestamp
				snap.Updated,
			)
			slog.Info("restored engine state", "updated", snap.Updated)
		} else {
			slog.Warn("snapshot too old, ignoring", "updated", snap.Updated)
		}
	} else {
		slog.Warn("no snapshot found", "error", err)
	}

	// Tick channel and engine consumer
	tickCh := make(chan engine.Tick, 1000)
	go engine.Run(ctx, tickCh, e)

	// Aggregator configuration
	relWin := time.Duration(cfg.ReliabilityWindowSec) * time.Second
	backoffInit := time.Duration(cfg.BackoffInitialMs) * time.Millisecond
	backoffMax := time.Duration(cfg.BackoffMaxMs) * time.Millisecond

	var primary oracle.Source

	switch strings.ToLower(cfg.AggregatorPrimary) {
	case "coingecko":
		primary = oracle.SourceCoingecko
	case "dexscreener":
		primary = oracle.SourceDexScreener
	default:
		primary = oracle.SourceJupiter
	}
	aggCfg := oracle.AggregatorConfig{
		Primary:           primary,
		ReliabilityWindow: relWin,
		BackoffInitial:    backoffInit,
		BackoffMax:        backoffMax,
	}
	pc := oracle.NewPriceClient(nil, cfg.CoingeckoAPIKey, cfg.CoingeckoKeyType)
	agg := oracle.NewAggregator(pc, aggCfg)

	// Start poller and snapshotter
	go engine.StartPoller(ctx, agg, tickCh, time.Duration(cfg.PollMs)*time.Millisecond)
	go persist.Start(ctx, store, e, time.Duration(cfg.SnapshotSec)*time.Second)
}

// livenessHandler handles /healthz requests.
// @Summary Health check
// @Description Returns 200 if service is healthy
// @Tags probes
// @Success 200
// @Router /healthz [get]
func livenessHandler(c echo.Context) error {
	return c.NoContent(http.StatusOK)
}

// readinessHandler handles /readyz requests.
// @Summary Readiness check
// @Description Returns 200 if service is ready
// @Tags probes
// @Success 200
// @Router /readyz [get]
func readinessHandler(c echo.Context) error {
	return c.NoContent(http.StatusOK)
}

// ratioHandler handles /ratio requests.
// @Summary Get fair price ratio
// @Description Returns the EMA-based fair price ratio, spot price, and sigma
// @Tags ratio
// @Produce json
// @Success 200 {object} RatioResponse
// @Router /ratio [get]
func ratioHandler(c echo.Context) error {
	// TODO: compute and return fair, spot, sigma, updated
	resp := RatioResponse{
		Fair:    0.0,
		Spot:    0.0,
		Sigma:   0.0,
		Updated: time.Now().Format(time.RFC3339),
	}
	return c.JSON(http.StatusOK, resp)
}

// metricsHandler handles /metrics requests.
// @Summary Prometheus metrics
// @Description Exposes Prometheus-compatible metrics
// @Tags metrics
// @Produce plain
// @Success 200
// @Router /metrics [get]
func metricsHandler(c echo.Context) error {
	promhttp.Handler().ServeHTTP(c.Response(), c.Request())
	return nil
}

// initHTTP sets up the Echo server with routes and middleware.
func initHTTP() *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.Use(middleware.Recover())
	e.Use(middleware.LoggerWithConfig(middleware.LoggerConfig{
		Skipper: func(c echo.Context) bool {
			p := c.Request().URL.Path
			return p == "/healthz" || p == "/readyz"
		},
	}))

	// Routes
	e.GET("/healthz", func(c echo.Context) error { return c.NoContent(http.StatusOK) })
	e.GET("/readyz", func(c echo.Context) error {
		// Snapshot now returns:
		//   emaBillySol, emaBillyUSD, emaSolUSD,
		//   sigmaBillySol, sigmaBillyUSD, sigmaSolUSD,
		//   ticks, updated
		_, _, _, _, _, _, _, updated := eInst.Snapshot()
		if time.Since(updated) > 30*time.Second {
			return c.NoContent(http.StatusServiceUnavailable)
		}
		return c.NoContent(http.StatusOK)
	})
	e.GET("/ratio", ratioHandler)
	e.GET("/metrics", metricsHandler)

	// Swagger
	e.GET("/swagger", func(c echo.Context) error { return c.Redirect(http.StatusMovedPermanently, "/swagger/index.html") })
	e.GET("/swagger/*", echoSwagger.WrapHandler)
	e.GET("/", func(c echo.Context) error { return c.Redirect(http.StatusFound, "/swagger/index.html") })

	return e
}

func main() {
	// Load configuration
	cfg, err := loadConfig("/etc/bb1.conf")
	if err != nil {
		slog.Error("configuration error", "error", err)
		os.Exit(1)
	}

	// Initialize logger
	initLogger(cfg.LogLevel)
	slog.Info("Starting service", "port", cfg.Port)

	// Initialize Redis
	rdb := initRedis(cfg.RedisURL)

	// Start pipeline (engine, polling, snapshot)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	startPipeline(ctx, cfg, rdb)

	// Start HTTP server
	e := initHTTP()
	addr := fmt.Sprintf(":%d", cfg.Port)
	if err := e.Start(addr); err != nil && err != http.ErrServerClosed {
		slog.Error("HTTP server error", "error", err)
		os.Exit(1)
	}

	<-ctx.Done()
	slog.Info("Shutting down server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.Shutdown(shutdownCtx)
	slog.Info("Server stopped")
}
