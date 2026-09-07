package auction_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/GoldFridge/factorflow/internal/auction"
)

func TestProfileClear(t *testing.T) {
	for _, size := range [][2]int{{10, 40}, {20, 80}, {40, 160}} {
		a, bids := benchmarkBatch(t, size[0], size[1])
		solver := auction.NewSolver()
		solver.Params.MaxBranchNodes = 1
		solver.Params.MaxRepairRounds = 0

		start := time.Now()
		sol, err := solver.Clear(a, bids, testClose)
		elapsed := time.Since(start)
		if err != nil {
			fmt.Println(size, "error:", err)
			continue
		}
		fmt.Printf("lots=%d bids=%d relaxed-only: %s allocations=%d repair=%d nodes=%d\n",
			size[0], size[1], elapsed, len(sol.Allocations), sol.RepairRounds, sol.BranchNodes)
	}
}
