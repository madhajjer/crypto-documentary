// order is a CLI for placing a single live order and measuring the round-trip
// latency from API call â†’ order acknowledged â†’ fully filled.
//
// Usage (run from indodax-bot/):
//
//	go run ./cmd/order <buy|sell> <pair> <amount> <price> [flags]
//
// Examples:
//
//	go run ./cmd/order buy  sol_idr 50000   3500000   # buy 50k IDR worth at 3,500,000
//	go run ./cmd/order sell sol_idr 0.01    3600000   # sell 0.01 SOL at 3,600,000
//
// Amount semantics:
//   - buy:  amount is in QUOTE currency (IDR)
//   - sell: amount is in BASE currency (e.g. SOL)
//
// Flags:
//
//	-timeout duration   give up waiting for fill after this long (default 60s)
//	-cancel              cancel the order if not filled before timeout (default true)
//	-log path            append a JSON record per run (default ../logs/order-roundtrip.ndjson)
//	-warmup duration     wait this long after subscribing to the private WS before placing (default 1s)
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/hajir/mm-bot/internal/client"
	"github.com/hajir/mm-bot/internal/models"
	"github.com/hajir/mm-bot/internal/roundtrip"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	_ = godotenv.Load()

	timeout := flag.Duration("timeout", 60*time.Second, "give up waiting for fill after this long")
	cancelOnTimeout := flag.Bool("cancel", true, "cancel the order if not filled before timeout")
	logPath := flag.String("log", "../logs/order-roundtrip.ndjson", "append JSON record per run")
	warmup := flag.Duration("warmup", 1*time.Second, "delay after subscribing private WS before placing order")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: order <buy|sell> <pair> <amount> <price> [flags]\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 4 {
		flag.Usage()
		os.Exit(2)
	}
	direction := strings.ToLower(flag.Arg(0))
	pair := strings.ToLower(flag.Arg(1))
	amount, err := strconv.ParseFloat(flag.Arg(2), 64)
	if err != nil {
		log.Fatalf("invalid amount %q: %v", flag.Arg(2), err)
	}
	price, err := strconv.ParseFloat(flag.Arg(3), 64)
	if err != nil {
		log.Fatalf("invalid price %q: %v", flag.Arg(3), err)
	}

	maxNotional := 100000.0
	if v := os.Getenv("ORDER_CLI_MAX_NOTIONAL_IDR"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			log.Fatalf("invalid ORDER_CLI_MAX_NOTIONAL_IDR=%q: %v", v, err)
		}
		maxNotional = n
	}

	apiKey := os.Getenv("INDODAX_API_KEY")
	secretKey := os.Getenv("INDODAX_SECRET_KEY")
	if apiKey == "" || secretKey == "" {
		log.Fatal("INDODAX_API_KEY and INDODAX_SECRET_KEY must be set")
	}

	priv := client.NewPrivateClient("https://indodax.com/tapi", apiKey, secretKey)
	pws := client.NewPrivateWSClient(
		"wss://pws.indodax.com/ws/",
		"https://indodax.com/api/private_ws/v1/generate_token",
		apiKey, secretKey,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := roundtrip.NewRunner(priv)
	pws.Subscribe(ctx, func(u models.OrderUpdate) {
		r.Dispatch(u)
	})

	stages, resCh := r.Run(ctx, roundtrip.Request{
		Direction:       direction,
		Pair:            pair,
		Amount:          amount,
		Price:           price,
		Timeout:         *timeout,
		CancelOnTimeout: *cancelOnTimeout,
		MaxNotionalIDR:  maxNotional,
		Warmup:          *warmup,
		LogPath:         *logPath,
	})

	log.Printf("placing %s %s amount=%v price=%v (warmup=%s timeout=%s)", direction, pair, amount, price, *warmup, *timeout)
	for s := range stages {
		log.Printf("event %-10s @ +%dms executed=%g remaining=%g", s.Name, s.ElapsedMS, s.Executed, s.Remaining)
	}
	rec := <-resCh

	fmt.Println()
	fmt.Printf("outcome:    %s\n", rec.Outcome)
	if rec.Error != "" {
		fmt.Printf("error:      %s\n", rec.Error)
	}
	fmt.Printf("order_id:   %s\n", rec.OrderID)
	fmt.Printf("place call: %d ms\n", rec.PlaceMS)
	if rec.FillMS > 0 {
		fmt.Printf("fill (DONE): %d ms\n", rec.FillMS)
	}
	fmt.Printf("total:      %d ms\n", rec.TotalMS)

	if rec.Outcome == "error" {
		os.Exit(1)
	}
}
