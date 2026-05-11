// mm-screener pulls Indodax public /api/summaries (one REST call, ~110KB,
// 512 IDR pairs) and prints a ranked shortlist of pairs whose ticker-level
// stats are *consistent with* the regime hfmm-bot's Avellaneda-Stoikov needs.
//
// It is a pre-filter for `mm-survey`, not a replacement: depth and mid-stddev
// still require the full poll. See notes/mm_screener_plan.md.
//
// Usage (run from indodax-bot/):
//
//	go run ./cmd/mm-screener
//	go run ./cmd/mm-screener -min-vol 500e6 -min-spread 0.010 -min-r2s 4 -top 10
//	go run ./cmd/mm-screener -survey-cmd
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

const summariesURL = "https://indodax.com/api/summaries"

type ticker struct {
	Buy        string `json:"buy"`
	Sell       string `json:"sell"`
	High       string `json:"high"`
	Low        string `json:"low"`
	Last       string `json:"last"`
	VolIDR     string `json:"vol_idr"`
	ServerTime int64  `json:"server_time"`
}

type summariesResp struct {
	Tickers    map[string]ticker          `json:"tickers"`
	Prices24h  map[string]json.RawMessage `json:"prices_24h"`
	Prices7d   map[string]json.RawMessage `json:"prices_7d"`
}

type row struct {
	Pair           string  `json:"pair"`
	Buy            float64 `json:"buy"`
	Sell           float64 `json:"sell"`
	High           float64 `json:"high"`
	Low            float64 `json:"low"`
	Last           float64 `json:"last"`
	VolIDR         float64 `json:"vol_idr_24h"`
	ServerTime     int64   `json:"server_time"`
	SpreadPct      float64 `json:"spread_pct"`
	RangePct       float64 `json:"range_pct"`
	Move24hPct     float64 `json:"move_24h_pct"`
	Move7dPct      float64 `json:"move_7d_pct"`
	RangeToSpread  float64 `json:"range_to_spread"`
	Score          float64 `json:"score"`
	Passed         bool    `json:"passed"`
	DropReason     string  `json:"drop_reason,omitempty"`
}

func parseFloatStr(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v
}

func parseRawNum(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	// may be string-quoted or bare number
	s := strings.Trim(string(raw), `"`)
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func main() {
	log.SetFlags(log.Ltime)

	minVol := flag.Float64("min-vol", 500e6, "minimum 24h IDR volume")
	minSpread := flag.Float64("min-spread", 0.010, "minimum top-of-book spread (fraction, e.g. 0.010 = 1.0%)")
	minR2S := flag.Float64("min-r2s", 4.0, "minimum range-to-spread ratio")
	maxStale := flag.Int("max-stale", 60, "max ticker staleness in seconds")
	topN := flag.Int("top", 10, "rows to print/write to top-N markdown")
	jsonOut := flag.Bool("json", false, "print full survivor JSON to stdout instead of table")
	surveyCmd := flag.Bool("survey-cmd", false, "print ready-to-paste mm-survey invocations for the top-N")
	dataDir := flag.String("data-dir", "data/mm_screener", "output directory")
	flag.Parse()

	resp, err := http.Get(summariesURL)
	if err != nil {
		log.Fatalf("GET %s: %v", summariesURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		log.Fatalf("GET %s: status %d: %s", summariesURL, resp.StatusCode, string(body))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatalf("read body: %v", err)
	}
	var s summariesResp
	if err := json.Unmarshal(body, &s); err != nil {
		log.Fatalf("decode summaries: %v", err)
	}

	now := time.Now().Unix()
	rows := make([]row, 0, len(s.Tickers))
	for pair, t := range s.Tickers {
		// IDR-only
		if !strings.HasSuffix(pair, "_idr") {
			continue
		}
		// exclude tokenized stocks (xidr xStock pattern e.g. aaplxidr_idr)
		base := strings.TrimSuffix(pair, "_idr")
		if strings.HasSuffix(base, "xidr") || base == "idrt" || base == "usdt" || base == "usdc" {
			continue
		}
		buy := parseFloatStr(t.Buy)
		sell := parseFloatStr(t.Sell)
		high := parseFloatStr(t.High)
		low := parseFloatStr(t.Low)
		last := parseFloatStr(t.Last)
		volIDR := parseFloatStr(t.VolIDR)

		r := row{
			Pair:       pair,
			Buy:        buy,
			Sell:       sell,
			High:       high,
			Low:        low,
			Last:       last,
			VolIDR:     volIDR,
			ServerTime: t.ServerTime,
		}

		if buy > 0 && sell > 0 {
			mid := (buy + sell) / 2
			r.SpreadPct = (sell - buy) / mid
		}
		if last > 0 {
			r.RangePct = (high - low) / last
		}
		if r.SpreadPct > 1e-9 {
			r.RangeToSpread = r.RangePct / r.SpreadPct
		}
		pairID := strings.ReplaceAll(pair, "_", "")
		if p24 := parseRawNum(s.Prices24h[pairID]); p24 > 0 && last > 0 {
			r.Move24hPct = last/p24 - 1
		}
		if p7 := parseRawNum(s.Prices7d[pairID]); p7 > 0 && last > 0 {
			r.Move7dPct = last/p7 - 1
		}

		// gates
		switch {
		case volIDR < *minVol:
			r.DropReason = fmt.Sprintf("vol_idr_24h %.0f < %.0f", volIDR, *minVol)
		case r.SpreadPct < *minSpread:
			r.DropReason = fmt.Sprintf("spread_pct %.4f < %.4f", r.SpreadPct, *minSpread)
		case r.RangeToSpread < *minR2S:
			r.DropReason = fmt.Sprintf("range_to_spread %.2f < %.2f", r.RangeToSpread, *minR2S)
		case t.ServerTime > 0 && now-t.ServerTime > int64(*maxStale):
			r.DropReason = fmt.Sprintf("stale: now-server_time = %ds > %ds", now-t.ServerTime, *maxStale)
		default:
			r.Passed = true
		}
		rows = append(rows, r)
	}

	survivors := make([]*row, 0)
	for i := range rows {
		if rows[i].Passed {
			survivors = append(survivors, &rows[i])
		}
	}

	// z-score over survivors only
	if len(survivors) > 0 {
		zs := func(get func(*row) float64) func(*row) float64 {
			n := float64(len(survivors))
			var mean, sumsq float64
			for _, r := range survivors {
				mean += get(r)
			}
			mean /= n
			for _, r := range survivors {
				d := get(r) - mean
				sumsq += d * d
			}
			std := math.Sqrt(sumsq / n)
			return func(r *row) float64 {
				if std == 0 {
					return 0
				}
				return (get(r) - mean) / std
			}
		}
		zR2S := zs(func(r *row) float64 { return r.RangeToSpread })
		zVol := zs(func(r *row) float64 { return r.VolIDR })
		zRng := zs(func(r *row) float64 { return r.RangePct })
		for _, r := range survivors {
			r.Score = 0.5*zR2S(r) + 0.3*zVol(r) + 0.2*zRng(r)
		}
	}

	sort.Slice(survivors, func(i, j int) bool { return survivors[i].Score > survivors[j].Score })

	// outputs
	unix := time.Now().Unix()
	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *dataDir, err)
	}
	jsonPath := filepath.Join(*dataDir, fmt.Sprintf("%d.json", unix))
	mdPath := filepath.Join(*dataDir, fmt.Sprintf("%d.md", unix))

	jf, err := os.Create(jsonPath)
	if err != nil {
		log.Fatalf("create %s: %v", jsonPath, err)
	}
	enc := json.NewEncoder(jf)
	enc.SetIndent("", "  ")
	out := struct {
		GeneratedAt   time.Time `json:"generated_at"`
		MinVolIDR     float64   `json:"min_vol_idr"`
		MinSpread     float64   `json:"min_spread_pct"`
		MinR2S        float64   `json:"min_range_to_spread"`
		Survivors     int       `json:"survivors"`
		Total         int       `json:"total"`
		Rows          []row     `json:"rows"`
	}{
		GeneratedAt: time.Now().UTC(),
		MinVolIDR:   *minVol,
		MinSpread:   *minSpread,
		MinR2S:      *minR2S,
		Survivors:   len(survivors),
		Total:       len(rows),
		Rows:        rows,
	}
	if err := enc.Encode(out); err != nil {
		log.Fatalf("encode json: %v", err)
	}
	jf.Close()

	limit := *topN
	if limit > len(survivors) {
		limit = len(survivors)
	}

	mdHeader := "| pair | spread% | range% | vol_idr (M) | r/s | 24h% | 7d% | score |\n"
	mdHeader += "|---|---|---|---|---|---|---|---|\n"
	mdBody := ""
	for _, r := range survivors[:limit] {
		mdBody += fmt.Sprintf("| %s | %.2f%% | %.2f%% | %.0f | %.1f | %+.1f%% | %+.1f%% | %.2f |\n",
			r.Pair, r.SpreadPct*100, r.RangePct*100, r.VolIDR/1e6, r.RangeToSpread,
			r.Move24hPct*100, r.Move7dPct*100, r.Score)
	}
	md := fmt.Sprintf("# MM screener — %s\n\nSurvivors: %d / %d total IDR pairs.\nGates: vol≥%.0fM, spread≥%.3f%%, r/s≥%.1f.\n\n%s%s",
		time.Now().UTC().Format(time.RFC3339), len(survivors), len(rows),
		*minVol/1e6, *minSpread*100, *minR2S, mdHeader, mdBody)
	if err := os.WriteFile(mdPath, []byte(md), 0o644); err != nil {
		log.Fatalf("write %s: %v", mdPath, err)
	}

	if *jsonOut {
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		fmt.Printf("MM screener — %d survivors / %d IDR pairs\n", len(survivors), len(rows))
		fmt.Printf("gates: vol≥%.0fM IDR, spread≥%.3f%%, r/s≥%.1f, max-stale %ds\n\n",
			*minVol/1e6, *minSpread*100, *minR2S, *maxStale)
		fmt.Print(mdHeader)
		fmt.Print(mdBody)
	}

	if *surveyCmd && limit > 0 {
		fmt.Println("\n# ready-to-paste mm-survey commands:")
		for _, r := range survivors[:limit] {
			fmt.Printf("go run ./cmd/mm-survey -pair %s -duration 30m -interval 3s\n", r.Pair)
		}
	}

	fmt.Fprintf(os.Stderr, "\nwrote %s\nwrote %s\n", jsonPath, mdPath)
}
