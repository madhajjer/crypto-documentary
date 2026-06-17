// lag-study reads pre-fetched 5m klines (Indodax IDR + USDT/IDR bridge, and
// Binance USD) for BTC/ENA/SOL/DOGE, measures the diurnal Indodax-vs-Binance
// fair-value gap, its catch-up, and the cost-charged out-of-sample expectancy,
// then writes a markdown/JSON report with an EDGE / MARGINAL / NO_EDGE verdict.
//
// Usage (run from indodax-bot/, after fetching data):
//
//	go run ./cmd/lag-study [-dir ../data] [-fee-bps 43] [-slip-bps 15]
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
	"strings"
	"time"

	"github.com/yourname/indodax-bot/lagstudy"
	"github.com/yourname/indodax-bot/models"
)

var coins = []string{"BTC", "ENA", "SOL", "DOGE"}

func loadKlines(path string) ([]models.Kline, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []models.Kline
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var k models.Kline
		if err := json.Unmarshal([]byte(line), &k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenTime.Before(out[j].OpenTime) })
	return out, nil
}

// gapsAreDiurnal returns true when the busiest-gap WIB hour is at least 1.5x the
// quietest populated hour — i.e. gaps are not flat across the clock.
func gapsAreDiurnal(stats []lagstudy.HourStat) bool {
	maxAbs, minAbs := 0.0, 1e18
	populated := 0
	for _, s := range stats {
		if s.N == 0 {
			continue
		}
		populated++
		if s.MeanAbs > maxAbs {
			maxAbs = s.MeanAbs
		}
		if s.MeanAbs < minAbs {
			minAbs = s.MeanAbs
		}
	}
	return populated >= 12 && minAbs > 0 && maxAbs/minAbs >= 1.5
}

// revertsPositive returns true when the gap meaningfully closes within the
// horizon (any measured point closes >= 25% of the entry gap on average).
func revertsPositive(curve []lagstudy.ReversionPoint) bool {
	for _, p := range curve {
		if p.N > 0 && p.MeanFrac >= 0.25 {
			return true
		}
	}
	return false
}

// verdict applies the spec's locked decision rule to the out-of-sample result.
func verdict(oos lagstudy.SimResult, minTrades int, diurnalPresent, reverts bool) string {
	switch {
	case !diurnalPresent || !reverts:
		return "NO_EDGE"
	case oos.ExpectancyPerTrade > 0 && oos.Trades >= minTrades:
		return "EDGE"
	case oos.ExpectancyPerTrade > 0:
		return "MARGINAL"
	default:
		return "NO_EDGE"
	}
}

type coinReport struct {
	Coin      string
	Stats     []lagstudy.HourStat
	Curve     []lagstudy.ReversionPoint
	Best      lagstudy.ConfigResult
	HasConfig bool
	OOS       lagstudy.SimResult
	Diurnal   bool
	Reverts   bool
	Verdict   string
	GapBars   int
}

func main() {
	dir := flag.String("dir", "../data", "kline data directory")
	feeBps := flag.Float64("fee-bps", 43, "round-trip fee in basis points")
	slipBps := flag.Float64("slip-bps", 15, "slippage per side in basis points")
	maxHold := flag.Int("max-hold", 12, "max bars to hold (12 x 5m = 1h)")
	maxK := flag.Int("max-k", 12, "reversion horizon in bars")
	minTrades := flag.Int("min-trades", 30, "min OOS trades for EDGE")
	outDir := flag.String("out", "../data/lag_study", "report output dir")
	flag.Parse()

	bridge, err := loadKlines(filepath.Join(*dir, "klines_USDTIDR_5.jsonl"))
	if err != nil {
		log.Fatalf("load USDT/IDR bridge: %v (fetch with cmd/fetch-klines -pair usdt_idr -tf 5)", err)
	}

	idr := map[string][]models.Kline{}
	usd := map[string][]models.Kline{}
	for _, c := range coins {
		ip := filepath.Join(*dir, fmt.Sprintf("klines_%sIDR_5.jsonl", c))
		bp := filepath.Join(*dir, fmt.Sprintf("binance_%sUSDT_5m.jsonl", c))
		ik, err := loadKlines(ip)
		if err != nil {
			log.Printf("skip %s: %v", c, err)
			continue
		}
		bk, err := loadKlines(bp)
		if err != nil {
			log.Printf("skip %s: %v", c, err)
			continue
		}
		idr[c], usd[c] = ik, bk
	}
	if len(idr) == 0 {
		log.Fatalf("no coin data loaded from %s", *dir)
	}

	bars := lagstudy.AlignSeries(lagstudy.Series{IndodaxIDR: idr, BinanceUSD: usd, USDTIDR: bridge})
	log.Printf("aligned %d bars across %d coins", len(bars), len(idr))

	base := lagstudy.SimParams{
		Exit: 0, MaxHold: *maxHold,
		FeeRoundTrip: *feeBps / 10000.0, SlippagePerSide: *slipBps / 10000.0,
	}
	entries := []float64{0.002, 0.004, 0.006, 0.010}
	windows := [][2]int{{-1, 0}, {0, 6}, {2, 8}, {22, 4}, {6, 12}}

	var reports []coinReport
	for _, c := range coins {
		if _, ok := idr[c]; !ok {
			continue
		}
		gaps := lagstudy.ComputeGaps(bars, c)
		if len(gaps) < 100 {
			log.Printf("skip %s: only %d gap bars", c, len(gaps))
			continue
		}
		first, second := lagstudy.SplitHalf(gaps)
		stats := lagstudy.DiurnalStats(gaps)
		curve := lagstudy.ReversionCurve(gaps, 0.004, *maxK)

		rep := coinReport{
			Coin: c, Stats: stats, Curve: curve, GapBars: len(gaps),
			Diurnal: gapsAreDiurnal(stats), Reverts: revertsPositive(curve),
		}
		best, ok := lagstudy.SelectBestConfig(first, base, entries, windows, *minTrades)
		rep.Best, rep.HasConfig = best, ok
		if ok {
			p := base
			p.Entry, p.HourStart, p.HourEnd = best.Entry, best.HourStart, best.HourEnd
			rep.OOS = lagstudy.SimulateExpectancy(second, p)
		}
		rep.Verdict = verdict(rep.OOS, *minTrades, rep.Diurnal, rep.Reverts)
		reports = append(reports, rep)
	}

	if err := writeReport(*outDir, *feeBps, *slipBps, len(bars), reports); err != nil {
		log.Fatalf("write report: %v", err)
	}
}

func writeReport(outDir string, feeBps, slipBps float64, alignedBars int, reports []coinReport) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	var md strings.Builder
	fmt.Fprintf(&md, "# Indodax–Binance Lag Study — %s\n\n", ts)
	fmt.Fprintf(&md, "Fee: %.0f bps round-trip · Slippage: %.0f bps/side · Aligned bars: %d · 5m resolution\n\n",
		feeBps, slipBps, alignedBars)
	md.WriteString("Expectancy is the market-neutral gap-convergence return, net of fee+slippage, measured out-of-sample (second half).\n\n")
	md.WriteString("## Verdict summary\n\n")
	md.WriteString("| Coin | GapBars | Diurnal? | Reverts? | OOS Trades | OOS Expectancy bps | WinRate | Verdict |\n")
	md.WriteString("|------|---------|----------|----------|------------|--------------------|---------|---------|\n")
	for _, r := range reports {
		fmt.Fprintf(&md, "| %s | %d | %v | %v | %d | %.1f | %.2f | %s |\n",
			r.Coin, r.GapBars, r.Diurnal, r.Reverts, r.OOS.Trades,
			r.OOS.ExpectancyPerTrade*10000, r.OOS.WinRate, r.Verdict)
	}
	for _, r := range reports {
		fmt.Fprintf(&md, "\n## %s — diurnal gap by WIB hour\n\n", r.Coin)
		md.WriteString("| WIB hour | N | MeanGap bps | MeanAbs bps |\n|---|---|---|---|\n")
		for _, s := range r.Stats {
			if s.N == 0 {
				continue
			}
			fmt.Fprintf(&md, "| %02d | %d | %.1f | %.1f |\n", s.Hour, s.N, s.MeanGap*10000, s.MeanAbs*10000)
		}
		if r.HasConfig {
			fmt.Fprintf(&md, "\nBest in-sample config: Entry=%.0f bps, WIB window [%d,%d), in-sample expectancy %.1f bps over %d trades.\n",
				r.Best.Entry*10000, r.Best.HourStart, r.Best.HourEnd,
				r.Best.InSample.ExpectancyPerTrade*10000, r.Best.InSample.Trades)
		} else {
			md.WriteString("\nNo in-sample config met the minimum-trades floor.\n")
		}
	}
	mdPath := filepath.Join(outDir, ts+".md")
	if err := os.WriteFile(mdPath, []byte(md.String()), 0o644); err != nil {
		return err
	}
	jb, _ := json.MarshalIndent(map[string]any{
		"runAt": time.Now().UTC(), "feeBps": feeBps, "slipBps": slipBps, "reports": reports,
	}, "", "  ")
	jsonPath := filepath.Join(outDir, ts+".json")
	if err := os.WriteFile(jsonPath, jb, 0o644); err != nil {
		return err
	}
	fmt.Printf("reports: %s , %s\n", mdPath, jsonPath)
	return nil
}
