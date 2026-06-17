// binance-screener fetches all Binance USDT pairs from the public /ticker/24hr
// endpoint (one call, ~200KB) and ranks them using percentile-based spread and
// range-to-spread gates.
//
// Filter order:
//  1. Keep *USDT symbols only (excluding stablecoins).
//  2. Drop pairs below -min-vol (absolute 24h USDT threshold).
//  3. Compute spread% and r/s percentiles over the volume-filtered universe
//     (so illiquid pairs with huge spreads don't inflate the threshold).
//  4. Keep pairs at/above -spread-pct AND -r2s-pct percentile.
//  5. Z-score rank survivors; output table + JSON + MD.
//
// Tries api.binance.com first, then api1–api4 as fallbacks.
//
// Usage (run from indodax-bot/):
//
//	go run ./cmd/binance-screener
//	go run ./cmd/binance-screener -min-vol 5e6 -spread-pct 80 -r2s-pct 60 -top 20
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var apiHosts = []string{
	"api.binance.com",
	"api1.binance.com",
	"api2.binance.com",
	"api3.binance.com",
	"api4.binance.com",
}

var stablecoins = map[string]bool{
	"USDCUSDT": true, "BUSDUSDT": true, "TUSDUSDT": true,
	"USDPUSDT": true, "FDUSDUSDT": true, "DAIUSDT": true,
	"EURUSDT": true, "GBPUSDT": true, "AUDUSDT": true,
}

type rawTicker struct {
	Symbol             string `json:"symbol"`
	PriceChangePercent string `json:"priceChangePercent"`
	LastPrice          string `json:"lastPrice"`
	BidPrice           string `json:"bidPrice"`
	AskPrice           string `json:"askPrice"`
	HighPrice          string `json:"highPrice"`
	LowPrice           string `json:"lowPrice"`
	QuoteVolume        string `json:"quoteVolume"`
}

type row struct {
	Symbol     string  `json:"symbol"`
	Bid        float64 `json:"bid"`
	Ask        float64 `json:"ask"`
	High       float64 `json:"high"`
	Low        float64 `json:"low"`
	Last       float64 `json:"last"`
	VolUSDT    float64 `json:"vol_usdt_24h"`
	Change24h  float64 `json:"change_24h_pct"`
	SpreadPct  float64 `json:"spread_pct"`
	RangePct   float64 `json:"range_pct"`
	R2S        float64 `json:"range_to_spread"`
	Score      float64 `json:"score"`
	Passed     bool    `json:"passed"`
	DropReason string  `json:"drop_reason,omitempty"`
}

func pf(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func percentile(sorted []float64, pct float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := pct / 100.0 * float64(len(sorted)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func fetchTickers(timeout time.Duration) ([]rawTicker, string, error) {
	client := &http.Client{Timeout: timeout}
	for _, host := range apiHosts {
		url := fmt.Sprintf("https://%s/api/v3/ticker/24hr", host)
		resp, err := client.Get(url)
		if err != nil {
			log.Printf("host %s: %v — trying next", host, err)
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			log.Printf("host %s: HTTP %d — trying next", host, resp.StatusCode)
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Printf("host %s: read error: %v — trying next", host, err)
			continue
		}
		var tickers []rawTicker
		if err := json.Unmarshal(body, &tickers); err != nil {
			log.Printf("host %s: decode error: %v — trying next", host, err)
			continue
		}
		return tickers, host, nil
	}
	return nil, "", fmt.Errorf("all Binance hosts unreachable: %v", apiHosts)
}

func main() {
	log.SetFlags(log.Ltime)

	minVol    := flag.Float64("min-vol", 1e6, "minimum 24h USDT volume")
	spreadPct := flag.Float64("spread-pct", 75, "keep pairs at/above this spread percentile (over vol-filtered universe)")
	r2sPct    := flag.Float64("r2s-pct", 50, "keep pairs at/above this range-to-spread percentile")
	topN      := flag.Int("top", 20, "rows to display")
	jsonOut   := flag.Bool("json", false, "print full JSON to stdout instead of table")
	dataDir   := flag.String("data-dir", "data/binance_screener", "output directory")
	flag.Parse()

	raw, host, err := fetchTickers(15 * time.Second)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("fetched %d tickers from %s", len(raw), host)

	// Step 1: USDT filter + compute metrics.
	rows := make([]row, 0, 300)
	for _, t := range raw {
		if !strings.HasSuffix(t.Symbol, "USDT") || stablecoins[t.Symbol] {
			continue
		}
		bid  := pf(t.BidPrice)
		ask  := pf(t.AskPrice)
		high := pf(t.HighPrice)
		low  := pf(t.LowPrice)
		last := pf(t.LastPrice)

		r := row{
			Symbol:    t.Symbol,
			Bid:       bid, Ask: ask, High: high, Low: low, Last: last,
			VolUSDT:   pf(t.QuoteVolume),
			Change24h: pf(t.PriceChangePercent),
		}
		if bid > 0 && ask > 0 {
			mid := (bid + ask) / 2
			r.SpreadPct = (ask - bid) / mid
		}
		if last > 0 {
			r.RangePct = (high - low) / last
		}
		if r.SpreadPct > 1e-12 {
			r.R2S = r.RangePct / r.SpreadPct
		}
		rows = append(rows, r)
	}

	// Step 2: volume gate (fixed threshold).
	volPass := make([]*row, 0, len(rows))
	for i := range rows {
		if rows[i].VolUSDT >= *minVol {
			volPass = append(volPass, &rows[i])
		} else {
			rows[i].DropReason = fmt.Sprintf("vol %.0f < %.0f", rows[i].VolUSDT, *minVol)
		}
	}

	// Step 3: percentile thresholds computed over vol-filtered universe.
	spreads := make([]float64, len(volPass))
	r2sVals := make([]float64, len(volPass))
	for i, r := range volPass {
		spreads[i] = r.SpreadPct
		r2sVals[i] = r.R2S
	}
	sort.Float64s(spreads)
	sort.Float64s(r2sVals)
	spreadThresh := percentile(spreads, *spreadPct)
	r2sThresh    := percentile(r2sVals, *r2sPct)

	// Step 4: apply percentile gates.
	survivors := make([]*row, 0)
	for _, r := range volPass {
		switch {
		case r.SpreadPct < spreadThresh:
			r.DropReason = fmt.Sprintf("spread %.4f%% < P%.0f=%.4f%%", r.SpreadPct*100, *spreadPct, spreadThresh*100)
		case r.R2S < r2sThresh:
			r.DropReason = fmt.Sprintf("r/s %.2f < P%.0f=%.2f", r.R2S, *r2sPct, r2sThresh)
		default:
			r.Passed = true
			survivors = append(survivors, r)
		}
	}

	// Step 5: z-score over survivors.
	if len(survivors) > 1 {
		zs := func(get func(*row) float64) func(*row) float64 {
			n := float64(len(survivors))
			var mean, sumsq float64
			for _, r := range survivors { mean += get(r) }
			mean /= n
			for _, r := range survivors { d := get(r) - mean; sumsq += d * d }
			std := math.Sqrt(sumsq / n)
			return func(r *row) float64 {
				if std == 0 { return 0 }
				return (get(r) - mean) / std
			}
		}
		zSpread := zs(func(r *row) float64 { return r.SpreadPct })
		zVol    := zs(func(r *row) float64 { return r.VolUSDT })
		zR2S    := zs(func(r *row) float64 { return r.R2S })
		for _, r := range survivors {
			r.Score = 0.5*zSpread(r) + 0.3*zVol(r) + 0.2*zR2S(r)
		}
	}
	sort.Slice(survivors, func(i, j int) bool { return survivors[i].Score > survivors[j].Score })

	// Output.
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *dataDir, err)
	}
	unix := time.Now().Unix()
	jsonPath := filepath.Join(*dataDir, fmt.Sprintf("%d.json", unix))
	mdPath   := filepath.Join(*dataDir, fmt.Sprintf("%d.md", unix))

	rowsCopy := make([]row, len(rows))
	for i := range rows { rowsCopy[i] = rows[i] }

	doc := struct {
		GeneratedAt   time.Time `json:"generated_at"`
		Source        string    `json:"source_host"`
		MinVolUSDT    float64   `json:"min_vol_usdt"`
		SpreadPctGate float64   `json:"spread_pct_gate"`
		SpreadThresh  float64   `json:"spread_threshold"`
		R2SPctGate    float64   `json:"r2s_pct_gate"`
		R2SThresh     float64   `json:"r2s_threshold"`
		Survivors     int       `json:"survivors"`
		VolFiltered   int       `json:"vol_filtered"`
		TotalUSDT     int       `json:"total_usdt_pairs"`
		Rows          []row     `json:"rows"`
	}{
		GeneratedAt:   time.Now().UTC(),
		Source:        host,
		MinVolUSDT:    *minVol,
		SpreadPctGate: *spreadPct,
		SpreadThresh:  spreadThresh,
		R2SPctGate:    *r2sPct,
		R2SThresh:     r2sThresh,
		Survivors:     len(survivors),
		VolFiltered:   len(volPass),
		TotalUSDT:     len(rows),
		Rows:          rowsCopy,
	}

	jf, err := os.Create(jsonPath)
	if err != nil { log.Fatalf("create %s: %v", jsonPath, err) }
	enc := json.NewEncoder(jf)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
	jf.Close()

	limit := *topN
	if limit > len(survivors) { limit = len(survivors) }

	mdHdr := "| symbol | spread% | range% | vol (M USDT) | r/s | 24h% | score |\n|---|---|---|---|---|---|---|\n"
	mdBody := ""
	for _, r := range survivors[:limit] {
		mdBody += fmt.Sprintf("| %s | %.3f%% | %.2f%% | %.1f | %.1f | %+.1f%% | %.2f |\n",
			r.Symbol, r.SpreadPct*100, r.RangePct*100, r.VolUSDT/1e6, r.R2S, r.Change24h, r.Score)
	}
	md := fmt.Sprintf(
		"# Binance screener — %s\n\nSource: %s\nSurvivors: %d / %d USDT pairs (vol≥%.0fk: %d).\nGates: spread≥P%.0f (%.4f%%), r/s≥P%.0f (%.2f).\n\n%s%s",
		time.Now().UTC().Format(time.RFC3339), host,
		len(survivors), len(rows), *minVol/1e3, len(volPass),
		*spreadPct, spreadThresh*100, *r2sPct, r2sThresh,
		mdHdr, mdBody,
	)
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		log.Fatalf("write %s: %v", mdPath, err)
	}

	if *jsonOut {
		_ = json.NewEncoder(os.Stdout).Encode(doc)
	} else {
		fmt.Printf("Binance screener — %d survivors / %d USDT pairs\n", len(survivors), len(rows))
		fmt.Printf("source: %s\n", host)
		fmt.Printf("gates: vol≥%.0fk USDT, spread≥P%.0f (%.4f%%), r/s≥P%.0f (%.2f)\n\n",
			*minVol/1e3, *spreadPct, spreadThresh*100, *r2sPct, r2sThresh)
		fmt.Print(mdHdr)
		fmt.Print(mdBody)
	}
	fmt.Fprintf(os.Stderr, "\nwrote %s\nwrote %s\n", jsonPath, mdPath)
}
