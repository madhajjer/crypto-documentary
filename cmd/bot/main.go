package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	hfmm "github.com/hajir/mm-bot/internal/hfmm"
	"github.com/hajir/mm-bot/internal/hfmm/exchange"

	"github.com/hajir/mm-bot/internal/client"
	"github.com/hajir/mm-bot/internal/config"
	"github.com/hajir/mm-bot/internal/models"
	"github.com/hajir/mm-bot/internal/recorder"
	"github.com/hajir/mm-bot/internal/roundtrip"
	"github.com/hajir/mm-bot/internal/runner"
	"github.com/hajir/mm-bot/internal/server"
	"github.com/hajir/mm-bot/internal/tracker"
)

// pairToSymbol converts "btc_idr" → "BTCIDR".
func pairToSymbol(pair string) string {
	out := make([]byte, 0, len(pair))
	for i := range pair {
		if pair[i] == '_' {
			continue
		}
		if pair[i] >= 'a' && pair[i] <= 'z' {
			out = append(out, pair[i]-32)
		} else {
			out = append(out, pair[i])
		}
	}
	return string(out)
}

// setupLogging tees log output to both stderr and a daily log file under logsDir.
func setupLogging(logsDir string) func() {
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		log.Printf("[main] could not create logs dir %s: %v", logsDir, err)
		return func() {}
	}
	filename := fmt.Sprintf("bot-%s.log", time.Now().Format("2006-01-02"))
	path := filepath.Join(logsDir, filename)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[main] could not open log file %s: %v", path, err)
		return func() {}
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
	log.Printf("[main] logging to %s", path)
	return func() { f.Close() }
}

// logMMParamChange appends one JSONL record to the param history file.
func logMMParamChange(path string, params map[string]interface{}) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("[mm] param_log: mkdir %s: %v", filepath.Dir(path), err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.Printf("[mm] param_log: open %s: %v", path, err)
		return
	}
	defer f.Close()
	rec := map[string]interface{}{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"params":    params,
	}
	if err := json.NewEncoder(f).Encode(rec); err != nil {
		log.Printf("[mm] param_log: write: %v", err)
	}
}

// mmController owns the lifecycle of the active MarketMakerRunner. It supports
// start/stop (via SetActive) and atomic rebuild on param/pair changes.
type mmController struct {
	mu       sync.Mutex
	active   bool
	pair     string
	cur      *runner.MarketMakerRunner
	cancel   context.CancelFunc
	parent   context.Context
	wg       sync.WaitGroup
	// priorPrices holds the strategy price buffer captured just before the
	// previous runner was stopped. Injected into the replacement instance to
	// avoid a sigma-warmup blind period on hot-reload.
	priorPrices []float64
}

func newMMController(parent context.Context, initialPair string) *mmController {
	return &mmController{parent: parent, pair: initialPair}
}

// install replaces the current runner. If active, it (re)starts the loop.
// It captures a state snapshot from the outgoing runner via stopLocked so the
// caller can inject it into the new instance (warm-start).
func (m *mmController) install(r *runner.MarketMakerRunner) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopLocked() // captures priorPrices from m.cur before stopping
	m.cur = r
	if m.active && r != nil {
		m.startLocked()
	}
}

// takePriorPrices returns and clears the captured price buffer from the last
// stopLocked call. Must be called while holding m.mu.
func (m *mmController) takePriorPricesLocked() []float64 {
	p := m.priorPrices
	m.priorPrices = nil
	return p
}

func (m *mmController) setActive(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v == m.active {
		return
	}
	m.active = v
	if v {
		if m.cur != nil {
			m.startLocked()
		}
	} else {
		m.stopLocked()
	}
}

func (m *mmController) startLocked() {
	ctx, cancel := context.WithCancel(m.parent)
	m.cancel = cancel
	r := m.cur
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := r.Start(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[mm] runner exited with error: %v", err)
		}
	}()
}

func (m *mmController) stopLocked() {
	if m.cancel != nil {
		// Capture the volatility state buffer before the runner exits so we
		// can warm-start the replacement and avoid a sigma-warmup blind period.
		if m.cur != nil {
			m.priorPrices = m.cur.StateSnapshot()
		}
		m.cancel()
		m.cancel = nil
		m.mu.Unlock()
		m.wg.Wait()
		m.mu.Lock()
	}
}

func (m *mmController) panic(ctx context.Context) {
	m.mu.Lock()
	r := m.cur
	m.mu.Unlock()
	if r != nil {
		r.Panic(ctx)
	}
}

func (m *mmController) setDryRun(v bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return false
	}
	m.cur.SetDryRun(v)
	return m.cur.DryRun()
}

func (m *mmController) dryRun() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur == nil {
		return false
	}
	return m.cur.DryRun()
}

func (m *mmController) getActive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active
}

func (m *mmController) getPair() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pair
}

func (m *mmController) setPair(p string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pair = p
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	closeLog := setupLogging(filepath.Join("..", "logs"))
	defer closeLog()

	cfg := config.Load()

	jar, _ := cookiejar.New(nil)
	sharedHTTP := &http.Client{
		Timeout: 15 * time.Second,
		Jar:     jar,
	}

	logsDir := filepath.Join("..", "logs")
	trk, err := tracker.New(logsDir, cfg.TradePair, cfg.CutLossPct)
	if err != nil {
		log.Fatalf("[main] tracker init: %v", err)
	}

	var lastPrice atomic.Value
	lastPrice.Store(float64(0))

	symbol := pairToSymbol(cfg.TradePair)
	extraPairs := make(map[string]struct{}, len(cfg.DashboardPairs)+len(cfg.AllowedPairs))
	for _, p := range cfg.DashboardPairs {
		extraPairs[p] = struct{}{}
	}
	for _, p := range cfg.AllowedPairs {
		extraPairs[p] = struct{}{}
	}
	delete(extraPairs, cfg.TradePair)
	extraSymbols := make([]string, 0, len(extraPairs))
	for p := range extraPairs {
		extraSymbols = append(extraSymbols, pairToSymbol(p))
	}
	hub := server.NewHub(symbol, extraSymbols...)
	hub.MMFillsPath = filepath.Join(cfg.DataDir, "mm_fills.jsonl")
	hub.Proxy = server.NewProxyHandler(
		cfg.BaseURLPublic,
		cfg.BaseURLPrivate,
		cfg.TradePair,
		cfg.APIKey,
		cfg.SecretKey,
		sharedHTTP,
	)

	hub.Proxy.GetTradeHistory = trk.GetHistory
	hub.GetTradeHistory = trk.GetHistory
	hub.SignalInitialCapitalIDR = cfg.SignalInitialCapitalIDR
	hub.MMInitialCapitalIDR = cfg.MMInitialCapitalIDR
	hub.Proxy.GetTradePosition = func() (models.Position, models.PnLSummary) {
		return trk.GetPosition(), trk.GetPnL(lastPrice.Load().(float64))
	}
	hub.Proxy.GetLastPrice = func() float64 {
		return lastPrice.Load().(float64)
	}
	trk.OnRecord(hub.BroadcastExecution)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Optional tick recorder (orderbook + trade snapshots from poll loop).
	var rec *recorder.Recorder
	if cfg.RecorderEnabled {
		r, err := recorder.New(cfg.DataDir, cfg.RecorderBookHz)
		if err != nil {
			log.Printf("[recorder] init failed: %v — disabled", err)
		} else {
			rec = r
			recStop := make(chan struct{})
			go rec.FlushLoop(recStop, 2*time.Second)
			defer func() {
				close(recStop)
				_ = rec.Close()
			}()
			log.Printf("[recorder] enabled: dir=%s book_hz=%.2f", filepath.Join(cfg.DataDir, "ticks"), cfg.RecorderBookHz)
		}
	}

	// Roundtrip handler — kept for /api/order/roundtrip endpoints.
	rtPriv := client.NewPrivateClient(cfg.BaseURLPrivate, cfg.APIKey, cfg.SecretKey)
	rtRunner := roundtrip.NewRunner(rtPriv)
	hub.Roundtrip = rtRunner
	hub.RoundtripEnabled = os.Getenv("ROUNDTRIP_API_ENABLED") == "true"
	hub.RoundtripLogPath = filepath.Join(logsDir, "order-roundtrip.ndjson")
	if v := os.Getenv("ORDER_CLI_MAX_NOTIONAL_IDR"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			hub.RoundtripMaxIDR = n
		}
	} else {
		hub.RoundtripMaxIDR = 100000
	}

	// Public REST poller for active pair + dashboard pairs (ticker + book + trades).
	mmPub := client.NewPublicClient(cfg.BaseURLPublic, sharedHTTP)
	mmPriv := client.NewPrivateClient(cfg.BaseURLPrivate, cfg.APIKey, cfg.SecretKey)
	wsPub := client.NewWSClient(cfg.BaseURLWebSocket)
	wsPub.SetJar(jar)
	if err := wsPub.Connect(ctx); err != nil {
		log.Printf("[mm] websocket connect failed, falling back to REST orderbook polling: %v", err)
	} else {
		defer wsPub.Close()
	}

	ctrl := newMMController(ctx, cfg.TradePair)
	ctrl.setActive(cfg.BotActive)

	var lastMMParams atomic.Value
	lastMMParams.Store(map[string]interface{}{})

	buildMM := func(pair string, params map[string]interface{}, priorPrices []float64) *runner.MarketMakerRunner {
		// Override pair in params so HFMMConfigFromParams sees the current pair.
		mp := make(map[string]interface{}, len(params)+1)
		for k, v := range params {
			mp[k] = v
		}
		mp["pair"] = pair
		mmCfg := runner.HFMMConfigFromParams(cfg, mp)

		eff := runner.NewEfficiencyTracker(
			time.Duration(cfg.HFMMEfficiencyWindowMin)*time.Minute,
			cfg.HFMMEfficiencyThreshold,
		)

		var mmTick int64
		hooks := hfmm.Hooks{
			OnQuote: func(e hfmm.QuoteEvent) {
				n := atomic.AddInt64(&mmTick, 1)
				if n == 1 || n%60 == 0 {
					log.Printf("[mm] tick #%d mid=%.0f bid=%.0f ask=%.0f spread=%.4f%% sigma=%.6f",
						n, e.Mid, e.Bid, e.Ask, e.SpreadPct*100, e.Sigma)
				}
				eff.ObserveMid(e.Mid)
				if warn, msg := eff.Check(); warn {
					log.Printf("[mm] EFFICIENCY WARNING: %s", msg)
					hub.BroadcastMMWarning(msg)
				}
				lastPrice.Store(e.Mid)
				hub.BroadcastQuote(e.Mid, e.Reservation, e.Bid, e.Ask, e.SpreadPct, e.Sigma)
			},
			OnFill: func(e hfmm.FillEvent) {
				log.Printf("[mm] fill side=%s price=%.0f amount=%.8f idr=%.0f order_id=%s",
					e.Side, e.Price, e.Amount, e.IDR, e.OrderID)
				hub.BroadcastFill(e.OrderID, e.Side, e.Price, e.Amount, e.IDR)
				server.AppendMMFill(hub.MMFillsPath, server.MMFillRecord{
					OrderID:   e.OrderID,
					Side:      e.Side,
					Price:     e.Price,
					Amount:    e.Amount,
					IDR:       e.IDR,
					Timestamp: e.Timestamp.UnixMilli(),
				})
				switch e.Side {
				case "buy":
					eff.ObserveBuy(e.Price)
				case "sell":
					eff.ObserveSell(e.Price)
				}
			},
			OnInventory: func(e hfmm.InventoryEvent) {
				hub.BroadcastInventory(e.WalletBTC, e.OpenBidBTC, e.TotalExposure, e.Target, e.Gap)
			},
			OnCycle: func() func(hfmm.CycleEvent) {
				var consecErrs int
				return func(e hfmm.CycleEvent) {
					if e.LastError == nil {
						consecErrs = 0
						return
					}
					consecErrs++
					log.Printf("[mm] cycle error (n=%d): %v", consecErrs, e.LastError)
					hub.BroadcastMMError(e.LastError.Error())
					if consecErrs >= 3 {
						sleep := time.Duration(consecErrs) * time.Second
						if sleep > 30*time.Second {
							sleep = 30 * time.Second
						}
						time.Sleep(sleep)
					}
				}
			}(),
			OnAdverseSelection: func(orderID, side string, fillPrice, currentMid float64) {
				log.Printf("[mm] adverse_selection order_id=%s side=%s fill=%.0f mid=%.0f",
					orderID, side, fillPrice, currentMid)
				hub.BroadcastMMAdverse(orderID, side, fillPrice, currentMid)
				server.AppendMMFill(hub.MMFillsPath, server.MMFillRecord{
					OrderID: orderID,
					Adverse: true,
				})
			},
			OnSkip: func(e hfmm.SkipEvent) {
				hub.BroadcastMMSkip(e.Reason, e.Imbalance)
			},
			OnRateLimitSkip: func(side string, dropped int) {
				msg := fmt.Sprintf("rate-limited: %d %s quotes dropped in last minute", dropped, side)
				log.Printf("[mm] %s", msg)
				hub.BroadcastMMWarning(msg)
			},
		}
		var opts []hfmm.Option
		if len(priorPrices) > 0 {
			opts = append(opts, hfmm.WithPriorState(priorPrices))
			log.Printf("[mm] warm-start: injecting %d prior price observations into new instance", len(priorPrices))
		}
		wsPub.SubscribeOrderBook(ctx, pair, func(models.OrderBook) {})
		return runner.NewMarketMaker(mmCfg, mmPub, mmPriv, func(p string) (*exchange.OrderBook, bool) {
			book, ok := wsPub.SnapshotOrderBook(p)
			if !ok || len(book.Bids) == 0 || len(book.Asks) == 0 {
				return nil, false
			}
			bids := make([][2]float64, len(book.Bids))
			for i, e := range book.Bids {
				bids[i] = [2]float64{e.Price, e.Amount}
			}
			asks := make([][2]float64, len(book.Asks))
			for i, e := range book.Asks {
				asks[i] = [2]float64{e.Price, e.Amount}
			}
			return &exchange.OrderBook{Bids: bids, Asks: asks}, true
		}, hooks, opts...)
	}

	// Hub control hooks.
	hub.SetBotStrategy = func(name string, params map[string]interface{}) error {
		if name != "hfmm" {
			return fmt.Errorf("only 'hfmm' strategy is supported in mm-bot; got %q", name)
		}
		lastMMParams.Store(params)
		// install() → stopLocked() captures the outgoing state into priorPrices.
		// We then read it back to warm-start the new instance.
		ctrl.mu.Lock()
		ctrl.stopLocked()
		saved := ctrl.takePriorPricesLocked()
		pair := ctrl.pair
		ctrl.cur = buildMM(pair, params, saved)
		if ctrl.active && ctrl.cur != nil {
			ctrl.startLocked()
		}
		ctrl.mu.Unlock()
		return nil
	}
	hub.SetBotActive = func(active bool) {
		ctrl.setActive(active)
	}
	hub.GetBotStatus = func() (bool, string) {
		return ctrl.getActive(), "hfmm"
	}
	hub.SetBotDryRun = func(v bool) (bool, bool) {
		applied := ctrl.setDryRun(v)
		return false, applied
	}
	hub.GetBotDryRun = func() (bool, bool) {
		return false, ctrl.dryRun()
	}
	hub.PanicMM = func() error {
		ctrl.setActive(false)
		pCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctrl.panic(pCtx)
		hub.BroadcastMMPanic("all orders cancelled")
		return nil
	}
	hub.GetBotPair = func() string { return ctrl.getPair() }
	hub.SetBotPair = func(newPair string) error {
		if !cfg.IsAllowedPair(newPair) {
			return fmt.Errorf("pair not allowed: %s", newPair)
		}
		mp, _ := lastMMParams.Load().(map[string]interface{})
		if mp == nil {
			mp = map[string]interface{}{}
		}
		ctrl.mu.Lock()
		oldPair := ctrl.pair
		ctrl.stopLocked() // captures priorPrices
		var saved []float64
		if oldPair == newPair {
			// Same instrument — safe to warm-start sigma.
			saved = ctrl.takePriorPricesLocked()
		} else {
			// Different instrument: sigma from old pair is meaningless for the new one.
			ctrl.priorPrices = nil
			log.Printf("[main] pair changed %s → %s: discarding prior sigma state", oldPair, newPair)
		}
		ctrl.pair = newPair
		ctrl.cur = buildMM(newPair, mp, saved)
		if ctrl.active && ctrl.cur != nil {
			ctrl.startLocked()
		}
		ctrl.mu.Unlock()
		log.Printf("[main] pair swap complete %s → %s", oldPair, newPair)
		hub.BroadcastPairChange(newPair)
		return nil
	}
	hub.OnMMParamChange = func(params map[string]interface{}) {
		logMMParamChange(filepath.Join(cfg.DataDir, "mm_param_history.jsonl"), params)
	}

	// Background ticker poller for active pair + dashboard pairs (feeds the
	// dashboard's 24h stats and last-price). Active pair is polled for cached
	// orderbook/trades that proxy endpoints serve.
	pollPairs := make(map[string]struct{})
	pollPairs[cfg.TradePair] = struct{}{}
	for _, p := range cfg.DashboardPairs {
		pollPairs[p] = struct{}{}
	}
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		poll := func() {
			for p := range pollPairs {
				if tk, err := mmPub.GetTicker(p); err == nil {
					hub.BroadcastTickerForSymbol(pairToSymbol(p), tk)
					if p == ctrl.getPair() {
						lastPrice.Store(tk.Last)
					}
				}
				if ob, err := mmPub.GetOrderBook(p); err == nil {
					hub.BroadcastOrderBookForSymbol(pairToSymbol(p), ob)
					if rec != nil {
						rec.WriteBook(p, ob)
					}
				}
				if trades, err := mmPub.GetRecentTrades(p); err == nil {
					for _, tr := range trades {
						hub.AddTradeForSymbol(pairToSymbol(p), tr)
						if rec != nil {
							rec.WriteTrade(p, tr)
						}
					}
				}
			}
		}
		poll()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				poll()
			}
		}
	}()

	go hub.Start(ctx, cfg.ServerAddr)

	log.Printf("[main] mm-bot started — BOT_ACTIVE=%v pair=%s", cfg.BotActive, cfg.TradePair)
	<-ctx.Done()
	ctrl.setActive(false)
	log.Println("bye.")
}
