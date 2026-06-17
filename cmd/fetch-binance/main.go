// fetch-binance paginates Binance klines forward and persists them to
// <dir>/binance_<SYMBOL>_<interval>.jsonl (SYMBOL e.g. BTCUSDT).
//
// Usage (run from indodax-bot/):
//
//	go run ./cmd/fetch-binance -pair btc_idr -interval 5m -days 90 [-dir ../data]
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yourname/indodax-bot/client"
	"github.com/yourname/indodax-bot/models"
)

var intervalDur = map[string]time.Duration{
	"1m": time.Minute, "5m": 5 * time.Minute, "15m": 15 * time.Minute,
	"1h": time.Hour, "1d": 24 * time.Hour,
}

func main() {
	log.SetFlags(log.Ltime)
	pair := flag.String("pair", "btc_idr", "Indodax-format pair (mapped to <BASE>USDT)")
	interval := flag.String("interval", "5m", "Binance interval: 1m 5m 15m 1h 1d")
	days := flag.Int("days", 90, "how many days back to fetch")
	base := flag.String("base", "https://api.binance.com/api/v3", "Binance API base URL")
	dir := flag.String("dir", "../data", "output data directory")
	flag.Parse()

	d, ok := intervalDur[*interval]
	if !ok {
		log.Fatalf("unsupported interval %q", *interval)
	}
	symbol := strings.ToUpper(strings.SplitN(*pair, "_", 2)[0]) + "USDT"
	outPath := filepath.Join(*dir, "binance_"+symbol+"_"+*interval+".jsonl")

	c := client.NewBinanceClient(*base)
	end := time.Now().UTC()
	start := end.Add(-time.Duration(*days) * 24 * time.Hour)

	var all []models.Kline
	cur := start
	for cur.Before(end) {
		batchEnd := cur.Add(1000 * d)
		if batchEnd.After(end) {
			batchEnd = end
		}
		ks, err := c.FetchKlines(*pair, *interval, cur.UnixMilli(), batchEnd.UnixMilli())
		if err != nil {
			log.Printf("fetch error at %s: %v — stopping", cur.Format("2006-01-02 15:04"), err)
			break
		}
		if len(ks) == 0 {
			cur = batchEnd
			continue
		}
		all = append(all, ks...)
		last := ks[len(ks)-1].OpenTime
		log.Printf("batch=%-5d total=%-7d | up to %s", len(ks), len(all), last.Format("2006-01-02 15:04"))
		cur = last.Add(d)
		time.Sleep(200 * time.Millisecond)
	}

	if len(all) == 0 {
		log.Println("No data fetched.")
		return
	}
	if err := saveKlines(outPath, all); err != nil {
		log.Fatalf("save failed: %v", err)
	}
	log.Printf("Saved %d bars to %s (%s → %s)", len(all), outPath,
		all[0].OpenTime.Format("2006-01-02"), all[len(all)-1].OpenTime.Format("2006-01-02"))
}

func saveKlines(path string, klines []models.Kline) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriterSize(f, 4*1024*1024)
	enc := json.NewEncoder(w)
	for _, k := range klines {
		if err := enc.Encode(k); err != nil {
			return err
		}
	}
	return w.Flush()
}
