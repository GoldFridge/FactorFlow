// Command factorflow runs the FactorFlow modular monolith.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "factorflow: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fmt.Printf("factorflow %s\n", version)
	return nil
}
