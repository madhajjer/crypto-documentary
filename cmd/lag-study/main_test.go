package main

import (
	"testing"

	"github.com/yourname/indodax-bot/lagstudy"
)

func TestVerdict(t *testing.T) {
	good := lagstudy.SimResult{Trades: 40, ExpectancyPerTrade: 0.002}
	if v := verdict(good, 30, true, true); v != "EDGE" {
		t.Errorf("want EDGE, got %s", v)
	}
	thin := lagstudy.SimResult{Trades: 10, ExpectancyPerTrade: 0.002}
	if v := verdict(thin, 30, true, true); v != "MARGINAL" {
		t.Errorf("want MARGINAL (too few trades), got %s", v)
	}
	neg := lagstudy.SimResult{Trades: 40, ExpectancyPerTrade: -0.001}
	if v := verdict(neg, 30, true, true); v != "NO_EDGE" {
		t.Errorf("want NO_EDGE (negative), got %s", v)
	}
	if v := verdict(good, 30, false, true); v != "NO_EDGE" {
		t.Errorf("want NO_EDGE (no diurnal), got %s", v)
	}
}
