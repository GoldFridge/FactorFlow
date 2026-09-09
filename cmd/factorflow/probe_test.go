package main

import (
	"fmt"
	"testing"

	"github.com/GoldFridge/factorflow/internal/platform/config"
)

func TestProbeIssuerSelection(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("live=%v account=%q network=%q\n",
		cfg.Providers.HederaIsLive(), cfg.Providers.HederaAccountID, cfg.Providers.HederaNetwork)

	chain := hederaClient(cfg)
	fmt.Printf("chain=%v issuer=%T\n", chain != nil, assetIssuer(cfg, chain))
}
