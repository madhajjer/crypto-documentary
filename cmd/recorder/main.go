// recorder is a standalone CLI that subscribes to Indodax public WS feeds
// (order book + trade activity) for one or more pairs and persists raw events
// via the recorder package, without booting the full indodax-bot server.
//
// Usage (run from indodax-bot/):
//
//	go run ./cmd/recorder [-pair btc_idr] [-extra eth_idr,sol_idr] [-bookhz 1] [-dir ./data]
//
// Stops cleanly on SIGINT/SIGTERM (flushes buffers, closes files).
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/yourname/indodax-bot/client"
	"github.com/yourname/indodax-bot/models"
	"github.com/yourname/indodax-bot/recorder"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	_ = godotenv.Load()

	pair := flag.String("pair", envOr("TRADE_PAIR", "btc_idr"), "primary pair to record (e.g. btc_idr)")
	extra := flag.String("extra", os.Getenv("DASHBOARD_PAIRS"), "comma-separated extra pairs to record")
	bookHz := flag.Float64("bookhz", parseFloat(envOr("RECORDER_BOOK_HZ", "1"), 1), "book snapshot rate cap per pair (0 = unlimited)")
	dir := flag.String("dir", envOr("DATA_DIR", "./data"), "data directory root; ticks are written under <dir>/ticks")
	wsURL := flag.String("ws", envOr("INDODAX_WS_URL", "wss://ws3.indodax.com/ws/"), "Indodax WS URL")
	binance := flag.String("binance", "", "Indodax-format pair to capture from Binance as fair-value ref (e.g. ena_idr); empty = off")
	binanceHz := flag.Float64("binancehz", 1.0, "Binance depth poll rate (Hz)")
	bookLevels := flag.Int("booklevels", 20, "order-book levels per side to persist")
	binanceURL := flag.String("binance-url", envOr("BINANCE_API_URL", "https://api.binance.com/api/v3"), "Binance REST base URL")
	flag.Parse()

	pairs := uniqueNonEmpty(append([]string{*pair}, splitCSV(*extra)...))
	if len(pairs) == 0 {
		log.Fatal("no pairs configured")
	}

	rec, err := recorder.New(*dir, *bookHz, *bookLevels)
	if err != nil {
		log.Fatalf("recorder init: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := make(chan struct{})
	go rec.FlushLoop(stop, 2*time.Second)

	ws := client.NewWSClient(*wsURL)
	if err := ws.Connect(ctx); err != nil {
		log.Fatalf("ws connect: %v", err)
	}

	for _, p := range pairs {
		p := p
		ws.SubscribeOrderBook(ctx, p, func(book models.OrderBook) {
			rec.WriteBook(p, &book)
		})
		ws.SubscribeTradeActivity(ctx, p, func(t models.Trade) {
			rec.WriteTrade(p, t)
		})
		log.Printf("[recorder] subscribed pair=%s", p)
	}

	if *binance != "" {
		bc := client.NewBinanceClient(*binanceURL)
		symbol := strings.ToLower(strings.SplitN(*binance, "_", 2)[0]) + "_usdt" // ena_idr -> ena_usdt
		hz := *binanceHz
		if hz <= 0 {
			hz = 1.0
		}
		go func() {
			ticker := time.NewTicker(time.Duration(float64(time.Second) / hz))
			defer ticker.Stop()
			var fails int
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					ob, err := bc.FetchDepth(*binance, *bookLevels)
					if err != nil {
						fails++
						if fails%30 == 1 { // log first, then every 30th
							log.Printf("[recorder] binance depth error (%d): %v", fails, err)
						}
						continue
					}
					fails = 0
					rec.WriteBinanceBook(symbol, ob)
				}
			}
		}()
		log.Printf("[recorder] binance leg: pair=%s symbol=%s hz=%.2f", *binance, symbol, hz)
	}

	log.Printf("[recorder] running: dir=%s book_hz=%.2f pairs=%v", *dir, *bookHz, pairs)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Printf("[recorder] shutting down")

	cancel()
	close(stop)
	ws.Close()
	if err := rec.Close(); err != nil {
		log.Printf("[recorder] close: %v", err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func uniqueNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func parseFloat(s string, def float64) float64 {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return v
	}
	return def
}
