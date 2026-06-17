// fill-study reads recorded Indodax ENA/IDR + USDT/IDR and Binance ENA/USDT
// order-book snapshots, detects deep-night WIB gaps vs Binance-fair, walks the
// real ask book to test whether a 100k-IDR clip is fillable below fair and
// reverts in-window, and writes a markdown report with the locked verdict.
//
// Usage (from indodax-bot/):
//
//	go run ./cmd/fill-study [-dir ../data/ticks] [-out ../data/fill_study]
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/yourname/indodax-bot/fillstudy"
)

func main() {
	dir := flag.String("dir", "../data/ticks", "directory of recorded *.jsonl tick files")
	out := flag.String("out", "../data/fill_study", "report output directory")
	flag.Parse()

	ena, err := fillstudy.LoadGlob(filepath.Join(*dir, "ena_idr_*.jsonl"))
	if err != nil {
		log.Fatalf("load ena: %v", err)
	}
	usdt, err := fillstudy.LoadGlob(filepath.Join(*dir, "usdt_idr_*.jsonl"))
	if err != nil {
		log.Fatalf("load usdt: %v", err)
	}
	bin, err := fillstudy.LoadGlob(filepath.Join(*dir, "binance_ena_usdt_*.jsonl"))
	if err != nil {
		log.Fatalf("load binance: %v", err)
	}
	log.Printf("loaded snaps: ena=%d usdt=%d binance=%d", len(ena), len(usdt), len(bin))

	frames := fillstudy.AlignFrames(ena, usdt, bin)
	p := fillstudy.Params{
		GapThresh: -0.01, TargetIDR: 100_000, FeeFrac: 0.0043,
		HoldSec: 1800, RevertFrac: 0.5, NightStart: 22, NightEnd: 4,
	}
	events := fillstudy.DetectEvents(frames, p)

	fillable := 0
	for _, e := range events {
		if e.Fillable {
			fillable++
		}
	}
	verdict := fillstudy.Verdict(fillable)

	if err := os.MkdirAll(*out, 0o755); err != nil {
		log.Fatalf("mkdir out: %v", err)
	}
	stamp := time.Now().UTC().Format("2006-01-02T150405Z")
	path := filepath.Join(*out, stamp+".md")
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("create report: %v", err)
	}
	defer f.Close()

	fmt.Fprintf(f, "# Stage 2 — ENA/IDR Fill-Quality Report\n\n")
	fmt.Fprintf(f, "Generated: %s\n\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(f, "Aligned frames: %d · Detected night events: %d · **Fillable: %d**\n\n", len(frames), len(events), fillable)
	fmt.Fprintf(f, "## Verdict: **%s** (threshold N>=5)\n\n", verdict)
	fmt.Fprintf(f, "| time (UTC) | gap bps | spread bps | vwap | fair | net conv bps | filled | reverted | fillable |\n")
	fmt.Fprintf(f, "|---|---:|---:|---:|---:|---:|:--:|:--:|:--:|\n")
	for _, e := range events {
		fmt.Fprintf(f, "| %s | %.0f | %.0f | %.2f | %.2f | %.0f | %t | %t | %t |\n",
			e.Time.Format("01-02 15:04"), e.GapBps, e.SpreadBps, e.VWAP, e.Fair, e.NetConvBps, e.Filled, e.Reverted, e.Fillable)
	}
	fmt.Fprintf(f, "\nTarget clip: 100k IDR · Fee: 43 bps · Hold: 30 min · Revert frac: 0.5\n")

	log.Printf("verdict=%s fillable=%d events=%d report=%s", verdict, fillable, len(events), path)
}
