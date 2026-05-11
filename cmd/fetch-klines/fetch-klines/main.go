// fetch-klines is a standalone CLI that paginates backward through
// Indodax history_v2 and persists klines to <dir>/klines_<SYMBOL>_<tf>.jsonl.
// Existing files are respected: the fetcher reads the earliest stored
// timestamp and continues backward from there (resume support).
//
// Usage (run from indodax-bot/):
//
//	go run ./cmd/fetch-klines -pair btc_idr -start 2013-01-01 [-tf 1] [-dir ../data]
//
// Supported tf values: 1, 5, 15, 60, 1D
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hajir/mm-bot/internal/models"
	"github.com/hajir/mm-bot/internal/server"
)

const (
	batchSize = 4800
	sleepMs   = 300
)

var tfDuration = map[string]time.Duration{
	"1":  time.Minute,
	"5":  5 * time.Minute,
	"15": 15 * time.Minute,
	"60": time.Hour,
	"1D": 24 * time.Hour,
}

type rawBar struct {
	Time   int64   `json:"Time"`
	Open   float64 `json:"Open"`
	High   float64 `json:"High"`
	Low    float64 `json:"Low"`
	Close  float64 `json:"Close"`
	Volume string  `json:"Volume"`
}

func main() {
	log.SetFlags(log.Ltime)

	pair := flag.String("pair", "btc_idr", "trading pair (e.g. btc_idr)")
	start := flag.String("start", "2013-01-01", "stop paginating before this date (YYYY-MM-DD)")
	tf := flag.String("tf", "1", "timeframe: 1, 5, 15, 60, 1D")
	dir := flag.String("dir", "../data", "output data directory")
	flag.Parse()

	d, ok := tfDuration[*tf]
	if !ok {
		log.Fatalf("unsupported tf=%q, valid: 1 5 15 60 1D", *tf)
	}
	target, err := time.Parse("2006-01-02", *start)
	if err != nil {
		log.Fatalf("invalid -start %q: %v", *start, err)
	}

	symbol := strings.ToUpper(strings.ReplaceAll(*pair, "_", ""))
	outPath := filepath.Join(*dir, fmt.Sprintf("klines_%s_%s.jsonl", symbol, *tf))

	proxy := server.NewProxyHandler("https://indodax.com", "", *pair, "", "", nil)

	existing := loadPersistedKlines(outPath)
	var earliest time.Time
	if len(existing) > 0 {
		earliest = existing[0].OpenTime
		log.Printf("Resuming: %d bars stored, earliest=%s",
			len(existing), earliest.Format("2006-01-02 15:04"))
	}

	var to time.Time
	if !earliest.IsZero() {
		to = earliest.Add(-d)
	} else {
		to = time.Now()
	}

	var fetched []models.Kline
	page := 0

	for to.After(target) {
		from := to.Add(-time.Duration(batchSize) * d)
		if from.Before(target) {
			from = target
		}

		raw, err := proxy.FetchOHLC(
			symbol, *tf,
			strconv.FormatInt(from.Unix(), 10),
			strconv.FormatInt(to.Unix(), 10),
		)
		if err != nil {
			log.Printf("page %d: fetch error: %v â€” stopping", page, err)
			break
		}

		bars := parseRawBars(raw)
		if len(bars) == 0 {
			log.Printf("page %d: 0 bars returned â€” no more history available", page)
			break
		}

		bars = dedup(bars, fetched)
		if len(bars) == 0 {
			to = from.Add(-d)
			continue
		}

		fetched = append(bars, fetched...)
		page++

		firstBar := bars[0].OpenTime
		lastBar := bars[len(bars)-1].OpenTime
		log.Printf("page %-3d | batch=%-5d total=%-7d | %s â†’ %s",
			page, len(bars), len(fetched)+len(existing),
			firstBar.Format("2006-01-02 15:04"),
			lastBar.Format("2006-01-02 15:04"),
		)

		to = firstBar.Add(-d)
		time.Sleep(time.Duration(sleepMs) * time.Millisecond)
	}

	if len(fetched) == 0 {
		log.Println("No new data fetched.")
		return
	}

	all := append(fetched, existing...)
	sort.Slice(all, func(i, j int) bool {
		return all[i].OpenTime.Before(all[j].OpenTime)
	})
	all = dedupSorted(all)

	if err := saveKlines(outPath, all); err != nil {
		log.Fatalf("save failed: %v", err)
	}

	firstAll := all[0].OpenTime
	lastAll := all[len(all)-1].OpenTime
	spanDays := lastAll.Sub(firstAll).Hours() / 24
	log.Printf("â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€")
	log.Printf("Saved %d bars to %s", len(all), outPath)
	log.Printf("Range: %s â†’ %s (%.0f days / %.1f years)",
		firstAll.Format("2006-01-02"),
		lastAll.Format("2006-01-02"),
		spanDays, spanDays/365.25)
	log.Printf("â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€â”€")
}

func parseRawBars(data []byte) []models.Kline {
	var bars []rawBar
	if err := json.Unmarshal(data, &bars); err != nil || len(bars) == 0 {
		return nil
	}
	klines := make([]models.Kline, len(bars))
	for i, b := range bars {
		vol, _ := strconv.ParseFloat(b.Volume, 64)
		klines[i] = models.Kline{
			OpenTime: time.Unix(b.Time, 0).UTC(),
			Open:     b.Open,
			High:     b.High,
			Low:      b.Low,
			Close:    b.Close,
			Volume:   vol,
		}
	}
	return klines
}

func dedup(newBars, existing []models.Kline) []models.Kline {
	seen := make(map[int64]struct{}, len(existing))
	for _, k := range existing {
		seen[k.OpenTime.Unix()] = struct{}{}
	}
	out := newBars[:0]
	for _, k := range newBars {
		if _, dup := seen[k.OpenTime.Unix()]; !dup {
			out = append(out, k)
		}
	}
	return out
}

func dedupSorted(klines []models.Kline) []models.Kline {
	if len(klines) == 0 {
		return klines
	}
	out := klines[:1]
	for _, k := range klines[1:] {
		if k.OpenTime.Unix() != out[len(out)-1].OpenTime.Unix() {
			out = append(out, k)
		}
	}
	return out
}

func loadPersistedKlines(path string) []models.Kline {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		log.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var klines []models.Kline
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var k models.Kline
		if err := json.Unmarshal(scanner.Bytes(), &k); err == nil {
			klines = append(klines, k)
		}
	}
	sort.Slice(klines, func(i, j int) bool {
		return klines[i].OpenTime.Before(klines[j].OpenTime)
	})
	return klines
}

func saveKlines(path string, klines []models.Kline) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
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
