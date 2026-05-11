// cancel_order is a CLI for cancelling a single open order on Indodax.
//
// Usage (run from indodax-bot/):
//
//	go run ./cmd/cancel_order <buy|sell> <pair> <order_id>
//
// Example:
//
//	go run ./cmd/cancel_order buy sol_idr 1234567
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/hajir/mm-bot/internal/client"
	"github.com/hajir/mm-bot/internal/models"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	_ = godotenv.Load()

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: cancel_order <buy|sell> <pair> <order_id>\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 3 {
		flag.Usage()
		os.Exit(2)
	}

	typeArg := strings.ToLower(flag.Arg(0))
	pair := strings.ToLower(flag.Arg(1))
	orderID := flag.Arg(2)

	var orderType models.OrderType
	switch typeArg {
	case "buy":
		orderType = models.OrderTypeBuy
	case "sell":
		orderType = models.OrderTypeSell
	default:
		log.Fatalf("invalid order type %q: must be buy or sell", typeArg)
	}

	apiKey := os.Getenv("INDODAX_API_KEY")
	secretKey := os.Getenv("INDODAX_SECRET_KEY")
	if apiKey == "" || secretKey == "" {
		log.Fatal("INDODAX_API_KEY and INDODAX_SECRET_KEY must be set")
	}

	priv := client.NewPrivateClient("https://indodax.com/tapi", apiKey, secretKey)

	log.Printf("cancelling %s order %s on %s", orderType, orderID, pair)
	start := time.Now()
	if err := priv.CancelOrder(pair, orderID, orderType); err != nil {
		fmt.Printf("outcome:  error\n")
		fmt.Printf("error:    %s\n", err)
		fmt.Printf("elapsed:  %d ms\n", time.Since(start).Milliseconds())
		os.Exit(1)
	}

	fmt.Printf("outcome:  cancelled\n")
	fmt.Printf("order_id: %s\n", orderID)
	fmt.Printf("pair:     %s\n", pair)
	fmt.Printf("type:     %s\n", orderType)
	fmt.Printf("elapsed:  %d ms\n", time.Since(start).Milliseconds())
}
