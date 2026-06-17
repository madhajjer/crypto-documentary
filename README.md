# MM-Bot Command Onboarding

This guide explains all available commands in mm-bot and how to use them. The workflow follows a **pair discovery pipeline**: filter candidates → survey survivors → run live MM strategy.

---

## 0. Prerequisites

**One-time setup:**

```bash
# Install Go 1.21+ and Node 18+
# Create .env with Indodax creds (at repo root)
INDODAX_KEY=your_api_key
INDODAX_SECRET=your_api_secret
INDODAX_BASEURL_PUBLIC=https://indodax.com/api/v2
INDODAX_BASEURL_PRIVATE=https://indodax.com/tapi
INDODAX_BASEURL_WEBSOCKET=wss://ws.indodax.com/websocket/v2
```

---

## 1. survey-all.ps1 — Parallel Discovery Orchestrator

**What it does:** Runs a full pair discovery pipeline end-to-end:
1. Runs `mm-screener` to filter ~512 IDR pairs down to promising candidates
2. Reads the screener output JSON
3. Spawns 5 parallel `mm-survey` processes (9-minute polls each)
4. Returns when all background surveys complete

**Why use it:**
- First time evaluating the bot on Indodax
- Periodic re-screening after market conditions shift
- No manual orchestration needed

**Usage:**
```powershell
# From repo root (Windows PowerShell):
.\survey-all.ps1
```

**Parallelization notes:**
- Uses `Start-Process` to spawn 5 background Go processes
- Runs for ~9 minutes (11+ levels × 4s interval, stays under 10-min / 180-req/min limit)
- Check results in `data/mm_survey/` when finished

**Output:**
```
data/
└── mm_survey/
    ├── btc_idr_<timestamp>/
    │   ├── samples.jsonl       # per-tick snapshots
    │   ├── summary.json        # aggregated metrics
    │   └── summary.md          # human-readable verdict (GO/TUNE/ABORT)
    ├── eth_idr_<timestamp>/
    └── ...
```

---

## 2. mm-screener — Ticker Filter

**What it does:** Pulls Indodax `/api/summaries` (one call, ~110KB) and ranks all 512 IDR pairs by spread, volume, and range metrics. Pre-filter for `mm-survey` — not a replacement.

**Metrics evaluated:**
- 24h IDR volume ≥ 500M (min-vol)
- Spread ≥ 1.0% (min-spread, prevents grinding)
- Range-to-spread ratio ≥ 4.0 (min-r2s, room for MM profit)
- Ticker staleness ≤ 60s (max-stale, data freshness)

**Usage:**

```bash
# Basic (uses defaults: top 10, min-vol=500M, min-spread=1.0%, min-r2s=4.0)
go run ./cmd/mm-screener/mm-screener

# Custom thresholds
go run ./cmd/mm-screener/mm-screener -min-vol 1e9 -min-spread 0.015 -min-r2s 5 -top 5

# Full JSON output (for scripting)
go run ./cmd/mm-screener/mm-screener -json

# Print ready-to-copy mm-survey commands
go run ./cmd/mm-screener/mm-screener -survey-cmd
```

**Flags:**
- `-min-vol` — minimum 24h IDR volume (default: 500e6)
- `-min-spread` — minimum spread fraction (default: 0.010 = 1.0%)
- `-min-r2s` — minimum range-to-spread ratio (default: 4.0)
- `-max-stale` — max ticker age in seconds (default: 60)
- `-top N` — show top N rows (default: 10)
- `-json` — output full JSON instead of table
- `-survey-cmd` — print `mm-survey` shell commands ready to paste
- `-data-dir` — output directory (default: data/mm_screener)

**Output:**
```
data/mm_screener/summaries_<timestamp>.json
```

---

## 3. mm-survey — Deep Pair Evaluation

**What it does:** Polls a single pair's orderbook + trades for X minutes (default: 9m). Measures spread, depth, volatility, and trade flow. Outputs a GO/TUNE/ABORT verdict.

**Key metrics:**
- Spread P50, P10, P90 (distribution)
- Spread time below min floor (0.7%) and friction threshold (0.62%)
- Top-5 bid/ask depth (liquidity for market orders)
- Mid-price volatility (σ in basis points)
- Trade frequency + volume per minute
- **Gross edge** = median spread − friction cost

**Usage:**

```bash
# Single survey (9 minutes, 4-second poll interval)
go run ./cmd/mm-survey/mm-survey -pair btc_idr -duration 9m -interval 4s

# Quick evaluation (2 minutes)
go run ./cmd/mm-survey/mm-survey -pair sol_idr -duration 2m -interval 2s

# Long soak (30 minutes for low-vol pairs)
go run ./cmd/mm-survey/mm-survey -pair shib_idr -duration 30m -interval 5s
```

**Flags:**
- `-pair` — trading pair (e.g., sol_idr, eth_idr) — **REQUIRED**
- `-duration` — polling duration (default: 9m; format: 10s, 5m, 1h)
- `-interval` — poll frequency (default: 4s; affects API call rate)

**Output:**
```
data/mm_survey/<pair>_<start_timestamp>/
├── samples.jsonl        # one JSON line per poll tick
├── summary.json         # aggregated stats
└── summary.md           # human-readable verdict
```

**Example summary.md excerpt:**
```
## Verdict: GO

Pair: sol_idr
- Spread median: 0.85% (well above min 0.7%)
- Gross edge: 0.23% (median spread 0.85% − friction 0.62%)
- Trade flow: 12 trades/min, 50M IDR/min
- Depth (top 5): 180M bid / 190M ask
- Volatility: 15 bp (manageable)

Recommendation: This pair has sufficient spread + depth for 
profitable market making. Ready to load into mm-bot.
```

---

## 4. bot — Live Market Maker

**What it does:** Runs the Avellaneda-Stoikov reservation price + spread strategy on a live pair. Manages orderbook polling, balance reconciliation, safety gates (toxic flow, adverse selection), and fill execution.

**Core responsibilities:**
- Maintain two-way quotes (bid/ask) updated every cycle
- Detect fills and update inventory
- Emit signals on imbalance (toxic flow detection)
- Serve a dashboard at `http://localhost:8080`
- WebSocket broadcasts to dashboard (quotes, fills, status)

**Prerequisites:**
- `.env` file with Indodax creds at repo root
- `internal/hfmm/config/presets/<pair>.env` or inline params

**Usage (from repo root):**

```bash
# Start the bot
go run ./cmd/bot/main.go

# Or with environment overrides
export BOT_ACTIVE=true
export BOT_INITIAL_PAIR=sol_idr
go run ./cmd/bot/main.go
```

**Configuration:**

Create `.env` at repo root (gitignored):
```env
INDODAX_KEY=your_key
INDODAX_SECRET=your_secret
INDODAX_BASEURL_PUBLIC=https://indodax.com/api/v2
INDODAX_BASEURL_PRIVATE=https://indodax.com/tapi
INDODAX_BASEURL_WEBSOCKET=wss://ws.indodax.com/websocket/v2

# MM parameters
BOT_ACTIVE=true
BOT_INITIAL_PAIR=btc_idr
BOT_RESERVATION_GAMMA=0.0005
BOT_RESERVATION_K=0.05
BOT_INVENTORY_TARGET=0.5
BOT_TOXIC_FLOW_THRESHOLD=0.15
```

**Dashboard access:**
```
http://localhost:8080
```

**Output logs:**
```
logs/bot-YYYY-MM-DD.log
data/mm_fills.jsonl       # fill records + adverse selections
data/mm_param_history.jsonl  # hot-reload param snapshots
```

**Lifecycle:**
- Start: reads `.env` + config, connects to Indodax + WebSocket, loads pair state
- Running: quotes + fills every cycle (~100ms)
- Hot-reload: pause → capture σ state → update params → resume (atomic)
- Stop: Ctrl+C cancels all open orders, closes connection

---

## 5. order — Single Order Placement & Latency Measurement

**What it does:** Places a single live order, waits for fill, measures round-trip latency from API call → order ack → fully filled.

**Purpose:**
- Baseline latency profile (e.g., "orders fill in 50-200ms")
- Test connectivity + Indodax API responsiveness
- Stress-test the order / fill detection loop

**Usage:**

```bash
# Buy 50,000 IDR worth of SOL at 3,500,000 IDR/SOL (waits up to 60s)
go run ./cmd/order/order buy sol_idr 50000 3500000

# Sell 0.01 SOL at 3,600,000 (waits 30s, auto-cancel if unfilled)
go run ./cmd/order/order sell sol_idr 0.01 3600000 -timeout 30s -cancel

# Quick test with 5-second timeout (will likely cancel)
go run ./cmd/order/order buy eth_idr 100000 25000000 -timeout 5s

# Append results to custom log
go run ./cmd/order/order buy sol_idr 50000 3500000 -log my_latencies.ndjson
```

**Flags:**
- `-timeout` — give up waiting after this duration (default: 60s)
- `-cancel` — cancel order if not filled before timeout (default: true)
- `-log` — path to append JSON record per run (default: ../logs/order-roundtrip.ndjson)
- `-warmup` — delay after subscribing to private WebSocket before placing (default: 1s)

**Amount semantics:**
- `buy <pair> <amount> <price>` — amount is **QUOTE** currency (IDR). Buy 50k IDR of SOL.
- `sell <pair> <amount> <price>` — amount is **BASE** currency (e.g., SOL). Sell 0.01 SOL.

**Output:**
```json
{
  "timestamp": "2026-05-11T12:34:56Z",
  "direction": "buy",
  "pair": "sol_idr",
  "amount": 50000,
  "limit_price": 3500000,
  "order_id": "1234567890",
  "placed_at": "2026-05-11T12:34:56.100Z",
  "filled_at": "2026-05-11T12:34:56.150Z",
  "latency_ms": 50,
  "final_price": 3499500,
  "status": "FILLED"
}
```

---

## 6. cancel_order — Single Order Cancellation

**What it does:** Sends a cancel request for an open order on Indodax. Useful for cleaning up stale quotes or emergency stops.

**Usage:**

```bash
# Cancel a buy order (given order_id from previous order call)
go run ./cmd/cancel_order/cancel_order buy sol_idr 1234567890

# Cancel a sell order
go run ./cmd/cancel_order/cancel_order sell btc_idr 9876543210
```

**Flags:** None (takes positional args only)

**Arguments:**
1. `<buy|sell>` — order side
2. `<pair>` — trading pair (e.g., sol_idr)
3. `<order_id>` — order ID from placement (string or int)

**Output:**
```
success: cancelled order 1234567890 on sol_idr (buy)
```

---

## 7. fetch-klines — Historical Candlestick Data

**What it does:** Downloads OHLCV bars from Indodax `/api/history_v2` and appends to a per-pair JSONL file. Supports resume (reads earliest existing timestamp, continues backward).

**Purpose:**
- Historical analysis + backtesting
- Volatility calculations
- Pair regime evaluation

**Usage:**

```bash
# Download SOL from genesis to 2026
go run ./cmd/fetch-klines/fetch-klines -pair sol_idr -start 2013-01-01 -tf 1

# Fetch 1-hour bars from start of 2024
go run ./cmd/fetch-klines/fetch-klines -pair btc_idr -start 2024-01-01 -tf 60

# Get 1-day bars
go run ./cmd/fetch-klines/fetch-klines -pair eth_idr -start 2020-01-01 -tf 1D

# Resume: reads existing file, continues backward from earliest bar
go run ./cmd/fetch-klines/fetch-klines -pair sol_idr -start 2013-01-01 -tf 5
```

**Flags:**
- `-pair` — trading pair (e.g., sol_idr) — **REQUIRED**
- `-start` — stop paginating before this date (YYYY-MM-DD format) — **REQUIRED**
- `-tf` — timeframe: 1, 5, 15, 60, 1D (default: 1)
- `-dir` — output directory (default: data)
- `-timeout` — subprocess timeout in seconds (default: 3600)

**Output:**
```
data/klines_<PAIR>_<TF>.jsonl
```

Each line is a parsed bar:
```json
{
  "time": 1673222400,
  "open": 24567.89,
  "high": 24890.12,
  "low": 24456.78,
  "close": 24750.45,
  "volume": "15.23456789"
}
```

---

## 8. recorder — Live Orderbook & Trade Capture

**What it does:** Subscribes to Indodax public WebSocket feeds (orderbook + trade activity) for one or more pairs and persists raw snapshots to JSONL. Optionally polls Binance depth as a fair-value reference leg. Stops cleanly on Ctrl+C (flushes buffers, closes files).

**Usage:**

```bash
# Record btc_idr orderbook + trades (1 snapshot/sec)
go run ./cmd/recorder -pair btc_idr

# Record multiple pairs
go run ./cmd/recorder -pair ena_idr -extra usdt_idr,sol_idr

# Record with Binance fair-value leg (for spread study)
go run ./cmd/recorder -pair ena_idr -extra usdt_idr -binance ena_idr

# Custom snapshot rate and book depth
go run ./cmd/recorder -pair btc_idr -bookhz 0.5 -booklevels 10 -dir ./data
```

**Flags:**
- `-pair` — primary pair to record (default: `btc_idr`)
- `-extra` — comma-separated additional pairs to record
- `-bookhz` — orderbook snapshot rate cap per pair in Hz (default: 1; 0 = unlimited)
- `-booklevels` — number of bid/ask levels to persist (default: 20)
- `-binance` — Indodax-format pair to capture from Binance as fair-value reference (e.g. `ena_idr`); empty = off
- `-binancehz` — Binance depth poll rate in Hz (default: 1.0)
- `-dir` — data directory root (default: `./data`)
- `-ws` — Indodax WebSocket URL

**Output:**
```
data/ticks/
├── ena_idr_<date>.jsonl        # Indodax orderbook + trade snapshots
├── usdt_idr_<date>.jsonl
└── binance_ena_usdt_<date>.jsonl  # Binance depth (if -binance set)
```

---

## 9. fill-study — Arb Fill Quality Analysis

**What it does:** Reads recorded Indodax + Binance orderbook snapshots, detects deep-night WIB price gaps vs Binance fair value, walks the real ask book to test whether a 100k-IDR clip is fillable below fair, checks if it reverts within 30 minutes, and writes a markdown report with a GO/NO_GO verdict.

**Prerequisites:** Run `recorder` with `-binance ena_idr` first to capture Indodax ENA/IDR, USDT/IDR, and Binance ENA/USDT snapshots.

**Usage:**

```bash
# Run against default data dir
go run ./cmd/fill-study

# Custom input/output directories
go run ./cmd/fill-study -dir ./data/ticks -out ./data/fill_study
```

**Flags:**
- `-dir` — directory of recorded `*.jsonl` tick files (default: `../data/ticks`)
- `-out` — report output directory (default: `../data/fill_study`)

**Study parameters (hardcoded):**
- Gap threshold: −1% vs Binance fair
- Target clip: 100k IDR
- Fee: 43 bps round-trip
- Hold window: 30 minutes
- Revert fraction: 0.5
- Night window: 22:00–04:00 WIB

**Output:**
```
data/fill_study/<timestamp>.md   # markdown report
```

**Example report excerpt:**
```markdown
## Verdict: GO (threshold N>=5)

Aligned frames: 1440 · Detected night events: 12 · Fillable: 7

| time (UTC) | gap bps | spread bps | vwap | fair | net conv bps | filled | reverted | fillable |
|---|---:|---:|---:|---:|---:|:--:|:--:|:--:|
| 06-10 22:14 | -120 | 35 | 14850.50 | 14960.00 | 42 | true | true | true |
```

---

## Typical Workflows

### Scenario 1: New Pair Evaluation (One-Time)

```bash
# 1. Filter candidates
go run ./cmd/mm-screener/mm-screener -top 5

# 2. Pick one from top-5, run 9-minute survey
go run ./cmd/mm-survey/mm-survey -pair sol_idr -duration 9m -interval 4s

# 3. Read data/mm_survey/sol_idr_<timestamp>/summary.md
# 4. If verdict is GO, load into mm-bot
```

### Scenario 2: Fast Pair Screening (Parallel)

```bash
# Runs full pipeline: screener → 5x parallel surveys → done
.\survey-all.ps1
# Check data/mm_survey/ in 10 minutes
```

### Scenario 3: Testing Order Latency

```bash
# Measure round-trip
go run ./cmd/order/order buy sol_idr 10000 3500000

# Repeat 10 times to build latency histogram
for i in {1..10}; do
  go run ./cmd/order/order buy sol_idr 10000 3500000 -timeout 10s
  sleep 2
done

# Analyze logs/order-roundtrip.ndjson
```

### Scenario 4: Live MM + Dashboard

```powershell
# Terminal 1: Start bot backend
go run ./cmd/bot/main.go

# Terminal 2: Start web dashboard
cd web
npm run dev

# Terminal 3: Open browser
# http://localhost:8080
```

---

## 10. binance-screener — Binance USDT Pair Filter

**What it does:** Fetches all Binance USDT pairs from `/ticker/24hr` (one call, ~200KB) and ranks them using **percentile-based** spread and range-to-spread gates. Thresholds adapt to current market conditions — no hardcoded spread floor. Falls back through `api1–api4.binance.com` if the primary host is blocked.

**Filter order:**
1. Keep `*USDT` symbols (excluding stablecoins)
2. Drop pairs below `-min-vol` (fixed 24h USDT threshold)
3. Compute spread% and r/s percentiles over the **volume-filtered** universe
4. Keep pairs at/above `-spread-pct` AND `-r2s-pct` percentile
5. Z-score rank survivors (0.5×spread + 0.3×vol + 0.2×r/s)

**Usage:**

```bash
# Default: vol≥1M USDT, spread≥P75, r/s≥P50, top 20
go run ./cmd/binance-screener

# Stricter spread gate, wider r/s, custom volume floor
go run ./cmd/binance-screener -min-vol 5e6 -spread-pct 80 -r2s-pct 60 -top 20

# Full JSON output
go run ./cmd/binance-screener -json
```

**Flags:**
- `-min-vol` — minimum 24h USDT volume (default: 1e6)
- `-spread-pct` — keep pairs at/above this spread percentile, computed over vol-filtered universe (default: 75)
- `-r2s-pct` — keep pairs at/above this range-to-spread percentile (default: 50)
- `-top N` — rows to display (default: 20)
- `-json` — print full JSON to stdout
- `-data-dir` — output directory (default: `data/binance_screener`)

**Output:**
```
data/binance_screener/<timestamp>.json
data/binance_screener/<timestamp>.md
```

**Example output:**
```
Binance screener — 9 survivors / 661 USDT pairs
source: api.binance.com
gates: vol≥1000k USDT, spread≥P75 (0.1225%), r/s≥P50 (122.26)

| symbol    | spread% | range%  | vol (M USDT) | r/s   | 24h%   | score |
|-----------|---------|---------|--------------|-------|--------|-------|
| DUSDT     | 0.379%  | 84.47%  | 3.8          | 223.0 | +3.9%  | 0.79  |
| SYNUSDT   | 0.243%  | 60.80%  | 8.3          | 250.2 | +60.3% | 0.41  |
| ASTERUSDT | 0.140%  | 20.62%  | 101.7        | 147.1 | +7.7%  | 0.41  |
```

**Note:** Binance spreads are ~10–50× tighter than Indodax. A P75 spread on Binance (~0.12%) signals a relatively illiquid pair on that exchange — useful for cross-exchange arb study rather than Binance MM.

---

## 11. fetch-binance — Binance Historical Klines

**What it does:** Paginates Binance klines forward from a start date and appends them to a per-symbol JSONL file. Used to fetch the Binance reference price series needed by `lag-study`.

**Usage:**

```bash
# Fetch 5m BTC/USDT klines from 2024
go run ./cmd/fetch-binance -pair btc_idr -start 2024-01-01 -tf 5m

# Fetch 1h ENA/USDT klines
go run ./cmd/fetch-binance -pair ena_idr -start 2024-01-01 -tf 1h

# Custom output dir
go run ./cmd/fetch-binance -pair sol_idr -start 2023-01-01 -tf 5m -dir ./data
```

**Flags:**
- `-pair` — Indodax-format pair (e.g. `ena_idr`); mapped automatically to Binance symbol (`ENAUSDT`)
- `-start` — fetch from this date (YYYY-MM-DD) — **REQUIRED**
- `-tf` — timeframe: `1m`, `5m`, `15m`, `1h`, `4h`, `1d` (default: `5m`)
- `-dir` — output directory (default: `data`)

**Output:**
```
data/binance_<SYMBOL>_<TF>.jsonl    # e.g. binance_ENAUSDT_5m.jsonl
```

Each line is a parsed bar:
```json
{"time": 1704067200, "open": 0.812, "high": 0.820, "low": 0.808, "close": 0.815, "volume": "123456.00"}
```

---

## 12. lag-study — Indodax-vs-Binance Lag Analysis

**What it does:** Reads pre-fetched 5m klines for Indodax IDR pairs + USDT/IDR bridge + Binance USD reference, measures the diurnal Indodax-vs-Binance fair-value gap and its catch-up speed, then computes out-of-sample cost-charged expectancy and outputs an **EDGE / MARGINAL / NO_EDGE** verdict.

**Prerequisites:** Run `fetch-klines` (Indodax 5m) and `fetch-binance` (Binance 5m) for the pairs you want to study.

**Usage:**

```bash
# Run against default data dir (studies BTC, ENA, SOL, DOGE)
go run ./cmd/lag-study

# Custom data directory
go run ./cmd/lag-study -dir ./data
```

**Flags:**
- `-dir` — directory containing kline JSONL files (default: `data`)

**Input files expected:**
```
data/klines_<PAIR>_5.jsonl          # Indodax 5m (e.g. klines_ena_idr_5.jsonl)
data/klines_usdt_idr_5.jsonl        # USDT/IDR bridge rate
data/binance_<SYMBOL>_5m.jsonl      # Binance 5m reference
```

**Output:**
```
data/lag_study_<timestamp>.json     # full metrics
data/lag_study_<timestamp>.md       # human-readable verdict report
```

**Example report excerpt:**
```markdown
## Verdict: EDGE

Pair: ENA/IDR
- Median gap vs Binance fair: -1.2% (Indodax lags)
- Catch-up half-life: ~15 min
- OOS expectancy (net fees): +0.38% per signal
- Coins studied: BTC, ENA, SOL, DOGE
```

---

## Emergency Stops

**To cancel all open orders without restarting the bot:**
```bash
# Use bot dashboard (UI button)
# Or use panic endpoint (if roundtrip API enabled)
# Or manually cancel via CLI
go run ./cmd/cancel_order/cancel_order buy btc_idr <order_id>
go run ./cmd/cancel_order/cancel_order sell btc_idr <order_id>
```

**To stop bot gracefully:**
```bash
# In terminal running bot: Ctrl+C
# Waits for in-flight orders, closes connection
```

---

## Common Issues

| Issue | Cause | Fix |
|-------|-------|-----|
| `parse .env: invalid line` | Typo in .env file | Check syntax: `KEY=value` on each line, no quotes |
| `403 Forbidden` | Bad API key/secret | Regenerate credentials on Indodax, update .env |
| `dial tcp: i/o timeout` | Network or Indodax down | Check `curl https://indodax.com/api/summaries` |
| `spread below min floor` | Low spread on pair (< 0.7%) | Switch to higher-spread pair (re-run screener) |
| `order not filled after 60s` | Illiquid or price moved | Increase timeout or check pair depth (mm-survey) |
| `port 8080 already in use` | Dashboard port conflict | Change port in config or kill existing process |

---

## Next Steps

1. **Run survey-all.ps1** to identify best pairs
2. **Pick a GO pair** and load into bot
3. **Monitor dashboard** for fills + efficiency warnings
4. **Adjust params** (gamma, k, inventory target) if needed
5. **Backtest** with historical data (fetch-klines + analysis)
