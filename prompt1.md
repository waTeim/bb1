# Reusable Prompt for a Code‑Generation Model

You are an experienced Go engineer.

Generate a production‑ready code that exactly follows the specification below.
► If any requirement is ambiguous or impossible, STOP and ASK for clarification.
► Do NOT invent external services, environment variables, or metrics that are not listed here.
► Keep functions short, cohesive, and fully documented.
► Favor clarity and idiomatic Go over premature optimisation.

## 1. Project goal

Build a Kubernetes‑ready REST service that maintains an EMA‑based “fair”
price ratio between the Solana tokens BILLY
(9Rhbn9G5poLvgnFzuYBtJgbzmiipNra35QpnUek9virt) and SOL (wrapped),
persisting state in Redis and exposing metrics & Swagger docs.

## 2. Mandatory tech stack

• HTTP router            : github.com/labstack/echo/v5           (latest)
• Swagger documentation  : github.com/swaggo/swag + echo-swagger (latest)
• Metrics                : github.com/prometheus/client_golang   (latest)
• Redis client           : github.com/redis/go-redis/v9          (latest)
• Logging                : go.uber.org/zap                       (latest)
• Container base image   : gcr.io/distroless/base-debian12 (run as non‑root)

## 3. Environment / configuration

PORT                = 8080          # HTTP listen port
POLL_MS             = 5000          # Jupiter polling interval (ms)
BACKUP_POLL_MS      = 15000,30000   # Coingecko,DexScreener intervals
EMA_WINDOW          = 288           # Number of ticks in EMA
REDIS_URL           = redis://redis:6379/0
SNAPSHOT_SEC        = 60            # Persist cadence (seconds)
DOCS_ENABLED        = true          # Serve /swagger/* when true
LOG_LEVEL           = info          # zap level

## 4. External price endpoints

Primary   : https://price.jup.ag/v2/price?id=<BILLY>&vsToken=<SOL>
Fallback  : https://api.coingecko.com/api/v3/simple/price?ids=billy-bets-by-virtuals&vs_currencies=sol
Tertiary  : https://api.dexscreener.com/latest/dex/search?q=BILLY/SOL

Fail‑over rules:
  - 2 consecutive Jupiter errors  → switch to Coingecko
  - 2 consecutive Coingecko errors → switch to DexScreener
  - All three down for >120 s      → mark service NotReady

## 5. Computation algorithm

• Maintain ring buffer of 6 888 ticks (float64 price, int64 timestamp).
• Compute rolling standard deviation σ over the same window.
• Clamp per‑tick delta to ±3 σ before feeding the EMA.
  EMA update: ema += α * (spot - ema) where α = 2 / (N + 1).
• On startup, load {ema, σ, buffer} from Redis; if snapshot older than
  24 h bootstrap from first live tick.

Redis snapshot keys (written atomically via Lua):
  fairratio:ema       float64
  fairratio:sigma     float64
  fairratio:ring      msgpacked []Tick
  fairratio:updated   epoch_ms

## 6. HTTP surface

GET /ratio     -> {"fair":0.0000332,"spot":0.0000334,"sigma":1.2e-6,"updated":"RFC3339"}
GET /healthz   -> 200 if last tick <30 s and upstream OK
GET /metrics   -> Prometheus exposition   (NOT in Swagger)
GET /swagger/* -> Swagger‑UI & raw spec (served only if DOCS_ENABLED=true)

Swagger annotations must fully describe success & error objects
(dto structs live in /internal/dto).

## 7. Prometheus metrics

fair_ratio                       gauge
spot_ratio                       gauge
ratio_sigma                      gauge
ticks_total{source}              counter
upstream_errors_total{source,code} counter
snapshot_duration_ms             histogram
http_request_duration_seconds{handler,method} histogram

## 8. Concurrency model

• 1 goroutine per upstream poller
• 1 goroutine consuming ticks -> EMA engine (single channel)
• 1 goroutine snapshotting Redis every SNAPSHOT_SEC
• HTTP server independent; graceful shutdown waits for in‑flight tick and
  final snapshot

## 9. Testing obligations

• Unit       : EMA & σ math; fail‑over state machine
• Integration: docker‑compose with mock Jupiter returning scripted prices
• Load       : 100 rps GET /ratio for 1 min; p95 latency <2 ms
• Lint       : go vet, staticcheck, golangci-lint run must pass

## 10. Build & CI targets

make generate  # swag init, update docs/*
make build     # produces bin/fairratio static binary
make docker    # builds distroless image fairratio:<git‑sha>
CI fails if docs/ changed but not committed (stale Swagger)
