// mm-survey polls Indodax public REST for an order book + recent trades on a
// given pair, computes the metrics that determine whether the hfmm-bot
// Avellaneda-Stoikov strategy can be profitable on it, and writes:
//
//   data/mm_survey/<pair>_<unixstart>/samples.jsonl   raw per-tick snapshots
//   data/mm_survey/<pair>_<unixstart>/summary.json    aggregated decision metrics
//   data/mm_survey/<pair>_<unixstart>/summary.md      human-readable verdict
//
// Usage (run from indodax-bot/):
//
//	go run ./cmd/mm-survey -pair ena_idr -duration 30m -interval 3s
//
// The summary.md ends with a GO / TUNE / ABORT verdict aligned to the
// MIN_SPREAD=0.007 floor and round-trip friction (~0.62%).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hajir/mm-bot/internal/client"
	"github.com/hajir/mm-bot/internal/models"
)

const (
	frictionPct = 0.0062 // round-trip taker friction (~2 Ã— 0.31%)
	minSpread   = 0.007  // hfmm-bot MIN_SPREAD floor
	depthLevels = 5
)

type sample struct {
	TS           time.Time `json:"ts"`
	BestBid      float64   `json:"best_bid"`
	BestAsk      float64   `json:"best_ask"`
	Mid          float64   `json:"mid"`
	SpreadPct    float64   `json:"spread_pct"`
	Top5BidIDR   float64   `json:"top5_bid_idr"`
	Top5AskIDR   float64   `json:"top5_ask_idr"`
	NewTrades    int       `json:"new_trades"`
	NewVolBaseQ  float64   `json:"new_vol_base"`
	NewVolIDR    float64   `json:"new_vol_idr"`
}

type summary struct {
	Pair             string    `json:"pair"`
	StartedAt        time.Time `json:"started_at"`
	EndedAt          time.Time `json:"ended_at"`
	DurationSec      float64   `json:"duration_sec"`
	Samples          int       `json:"samples"`

	SpreadMedianPct  float64   `json:"spread_median_pct"`
	SpreadP10Pct     float64   `json:"spread_p10_pct"`
	SpreadP90Pct     float64   `json:"spread_p90_pct"`
	SpreadBelowFloor float64   `json:"spread_below_min_floor_share"`
	SpreadBelowFric  float64   `json:"spread_below_friction_share"`

	Depth5BidMedIDR  float64   `json:"depth5_bid_median_idr"`
	Depth5AskMedIDR  float64   `json:"depth5_ask_median_idr"`

	TradesPerMin     float64   `json:"trades_per_min"`
	VolPerMinIDR     float64   `json:"vol_per_min_idr"`

	MidStdDevBP      float64   `json:"mid_stddev_bp"` // basis points (1bp = 0.01%)
	MidReturnStdBP   float64   `json:"mid_return_stddev_bp"`

	GrossEdgePct     float64   `json:"gross_edge_pct"` // median spread âˆ’ friction
	Verdict          string    `json:"verdict"`
	Notes            []string  `json:"notes"`
}

func main() {
	log.SetFlags(log.Ltime)

	pair := flag.String("pair", "ena_idr", "trading pair, e.g. ena_idr / sol_idr")
	dur := flag.Duration("duration", 30*time.Minute, "total survey duration")
	interval := flag.Duration("interval", 3*time.Second, "poll interval (>=1s)")
	baseURL := flag.String("base", "https://indodax.com", "Indodax REST base")
	outDir := flag.String("out", "../data/mm_survey", "output directory root")
	flag.Parse()

	if *interval < time.Second {
		log.Fatalf("interval must be >= 1s (got %s)", *interval)
	}

	pub := client.NewPublicClient(*baseURL, nil)

	start := time.Now()
	runDir := filepath.Join(*outDir, fmt.Sprintf("%s_%d", *pair, start.Unix()))
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", runDir, err)
	}
	samplesPath := filepath.Join(runDir, "samples.jsonl")
	f, err := os.Create(samplesPath)
	if err != nil {
		log.Fatalf("create %s: %v", samplesPath, err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)

	log.Printf("survey %s for %s every %s â†’ %s", *pair, *dur, *interval, runDir)

	deadline := start.Add(*dur)
	tick := time.NewTicker(*interval)
	defer tick.Stop()

	seenTID := map[string]struct{}{}
	var samples []sample
	totalTrades := 0
	totalVolIDR := 0.0
	logEvery := 10

	// Prime with current trades so we don't count the entire history on tick 1.
	if trades, err := pub.GetRecentTrades(*pair); err == nil {
		for _, t := range trades {
			seenTID[t.TID] = struct{}{}
		}
	}

	pollOnce := func() {
		now := time.Now()
		book, err := pub.GetOrderBook(*pair)
		if err != nil {
			log.Printf("orderbook err: %v", err)
			return
		}
		if len(book.Bids) == 0 || len(book.Asks) == 0 {
			log.Printf("empty book â€” skip")
			return
		}
		bid := book.Bids[0].Price
		ask := book.Asks[0].Price
		mid := (bid + ask) / 2
		s := sample{
			TS:         now,
			BestBid:    bid,
			BestAsk:    ask,
			Mid:        mid,
			SpreadPct:  (ask - bid) / mid,
			Top5BidIDR: depthIDR(book.Bids, depthLevels),
			Top5AskIDR: depthIDR(book.Asks, depthLevels),
		}

		trades, err := pub.GetRecentTrades(*pair)
		if err == nil {
			for _, t := range trades {
				if _, dup := seenTID[t.TID]; dup {
					continue
				}
				seenTID[t.TID] = struct{}{}
				s.NewTrades++
				s.NewVolBaseQ += t.Amount
				s.NewVolIDR += t.Amount * t.Price
			}
			totalTrades += s.NewTrades
			totalVolIDR += s.NewVolIDR
		}

		samples = append(samples, s)
		_ = enc.Encode(s)

		if len(samples)%logEvery == 0 {
			log.Printf("[%4d] spread=%.3f%% mid=%.0f top5=(%.0f/%.0f) trades+%d",
				len(samples), s.SpreadPct*100, s.Mid, s.Top5BidIDR, s.Top5AskIDR, s.NewTrades)
		}
	}

	pollOnce()
	for {
		select {
		case <-tick.C:
			if time.Now().After(deadline) {
				goto done
			}
			pollOnce()
		}
	}
done:
	_ = w.Flush()

	end := time.Now()
	sum := buildSummary(*pair, start, end, samples, totalTrades, totalVolIDR)

	sumPath := filepath.Join(runDir, "summary.json")
	if err := writeJSON(sumPath, sum); err != nil {
		log.Fatalf("write summary.json: %v", err)
	}
	mdPath := filepath.Join(runDir, "summary.md")
	if err := os.WriteFile(mdPath, []byte(renderMD(sum)), 0o644); err != nil {
		log.Fatalf("write summary.md: %v", err)
	}

	log.Printf("done. samples=%d verdict=%s", len(samples), sum.Verdict)
	log.Printf("artifacts: %s", runDir)
}

func depthIDR(levels []models.OrderBookEntry, n int) float64 {
	if len(levels) < n {
		n = len(levels)
	}
	var sum float64
	for i := 0; i < n; i++ {
		sum += levels[i].Price * levels[i].Amount
	}
	return sum
}

func buildSummary(pair string, start, end time.Time, ss []sample, totalTrades int, totalVolIDR float64) summary {
	dur := end.Sub(start).Seconds()
	out := summary{Pair: pair, StartedAt: start, EndedAt: end, DurationSec: dur, Samples: len(ss)}
	if len(ss) == 0 {
		out.Verdict = "NO_DATA"
		return out
	}

	spreads := make([]float64, len(ss))
	bidDepths := make([]float64, len(ss))
	askDepths := make([]float64, len(ss))
	mids := make([]float64, len(ss))
	belowFloor, belowFric := 0, 0
	for i, s := range ss {
		spreads[i] = s.SpreadPct
		bidDepths[i] = s.Top5BidIDR
		askDepths[i] = s.Top5AskIDR
		mids[i] = s.Mid
		if s.SpreadPct < minSpread {
			belowFloor++
		}
		if s.SpreadPct < frictionPct {
			belowFric++
		}
	}
	n := float64(len(ss))
	out.SpreadMedianPct = pct(spreads, 0.50)
	out.SpreadP10Pct = pct(spreads, 0.10)
	out.SpreadP90Pct = pct(spreads, 0.90)
	out.SpreadBelowFloor = float64(belowFloor) / n
	out.SpreadBelowFric = float64(belowFric) / n
	out.Depth5BidMedIDR = pct(bidDepths, 0.50)
	out.Depth5AskMedIDR = pct(askDepths, 0.50)

	if dur > 0 {
		out.TradesPerMin = float64(totalTrades) / (dur / 60.0)
		out.VolPerMinIDR = totalVolIDR / (dur / 60.0)
	}

	out.MidStdDevBP = stddev(mids) / mean(mids) * 10000
	out.MidReturnStdBP = returnStdDev(mids) * 10000

	out.GrossEdgePct = out.SpreadMedianPct - frictionPct
	out.Verdict, out.Notes = decide(out)
	return out
}

func decide(s summary) (string, []string) {
	var notes []string
	if s.SpreadBelowFloor > 0.50 {
		notes = append(notes, fmt.Sprintf("spread < %.2f%% MIN_SPREAD floor in %.0f%% of ticks â€” fee_guard will dominate", minSpread*100, s.SpreadBelowFloor*100))
	}
	if s.GrossEdgePct <= 0 {
		notes = append(notes, "median spread does not cover round-trip friction â€” structurally negative-EV")
		return "ABORT", notes
	}
	if s.TradesPerMin < 0.5 {
		notes = append(notes, fmt.Sprintf("only %.2f trades/min â€” fills will be sparse, inventory_age gate will trigger often", s.TradesPerMin))
	}
	if s.Depth5BidMedIDR < 5_000_000 || s.Depth5AskMedIDR < 5_000_000 {
		notes = append(notes, fmt.Sprintf("thin top-5 depth (bid med %.0f IDR / ask med %.0f IDR) â€” keep ORDER_SIZE small", s.Depth5BidMedIDR, s.Depth5AskMedIDR))
	}
	if s.SpreadBelowFloor > 0.30 || s.GrossEdgePct < 0.003 {
		return "TUNE", notes
	}
	notes = append(notes, fmt.Sprintf("median spread %.3f%% covers friction with %.3f%% gross edge", s.SpreadMedianPct*100, s.GrossEdgePct*100))
	return "GO", notes
}

func renderMD(s summary) string {
	w := &writer{}
	w.f("# MM survey â€” %s\n\n", s.Pair)
	w.f("Window: %s â†’ %s (%.1f min, %d samples)\n\n",
		s.StartedAt.Format("2006-01-02 15:04"),
		s.EndedAt.Format("15:04"), s.DurationSec/60, s.Samples)
	w.f("## Spread\n")
	w.f("- median: **%.3f%%** (p10 %.3f%% / p90 %.3f%%)\n", s.SpreadMedianPct*100, s.SpreadP10Pct*100, s.SpreadP90Pct*100)
	w.f("- below MIN_SPREAD %.2f%%: %.1f%% of ticks\n", minSpread*100, s.SpreadBelowFloor*100)
	w.f("- below friction %.2f%%: %.1f%% of ticks\n\n", frictionPct*100, s.SpreadBelowFric*100)
	w.f("## Depth (top-5)\n- bid median: %.0f IDR\n- ask median: %.0f IDR\n\n", s.Depth5BidMedIDR, s.Depth5AskMedIDR)
	w.f("## Flow\n- trades/min: %.2f\n- IDR vol/min: %.0f\n\n", s.TradesPerMin, s.VolPerMinIDR)
	w.f("## Volatility\n- mid stddev: %.1f bp\n- 1-tick return stddev: %.1f bp\n\n", s.MidStdDevBP, s.MidReturnStdBP)
	w.f("## Edge\n- gross edge (median spread âˆ’ friction): **%.3f%%**\n\n", s.GrossEdgePct*100)
	w.f("## Verdict: **%s**\n", s.Verdict)
	for _, n := range s.Notes {
		w.f("- %s\n", n)
	}
	return w.s
}

type writer struct{ s string }

func (w *writer) f(format string, a ...interface{}) { w.s += fmt.Sprintf(format, a...) }

func writeJSON(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

func pct(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := append([]float64(nil), xs...)
	sort.Float64s(c)
	idx := int(math.Round(q * float64(len(c)-1)))
	return c[idx]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	m := mean(xs)
	s := 0.0
	for _, x := range xs {
		d := x - m
		s += d * d
	}
	return math.Sqrt(s / float64(len(xs)))
}

func returnStdDev(mids []float64) float64 {
	if len(mids) < 3 {
		return 0
	}
	rets := make([]float64, 0, len(mids)-1)
	for i := 1; i < len(mids); i++ {
		if mids[i-1] == 0 {
			continue
		}
		rets = append(rets, (mids[i]-mids[i-1])/mids[i-1])
	}
	return stddev(rets)
}
