package auction_test

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/skimer2king/factorflow/internal/auction"
	"github.com/skimer2king/factorflow/internal/platform/money"
	"github.com/skimer2king/factorflow/internal/risk"
)

// benchmarkBatch builds the batch size the specification targets: a pool of invoices and a
// larger pool of constrained bids, from a fixed seed so runs are comparable.
func benchmarkBatch(tb testing.TB, lotCount, bidCount int) (*auction.Auction, []*auction.Bid) {
	tb.Helper()

	rng := rand.New(rand.NewSource(20260905))
	issuers := make([]uuid.UUID, 25)
	for i := range issuers {
		issuers[i] = seqUUID(0x60, i+1)
	}

	lots := make([]auction.Lot, 0, lotCount)
	for i := range lotCount {
		face := 5000 + rng.Intn(40)*500
		discount := 150 + rng.Intn(700)
		lots = append(lots, lot(i+1, fmt.Sprintf("DEBTOR-%d", rng.Intn(60)), issuers[rng.Intn(len(issuers))],
			fmt.Sprintf("%d.00", face), fmt.Sprintf("%d.00", face-discount),
			[]risk.Grade{risk.GradeA, risk.GradeB, risk.GradeC, risk.GradeD}[rng.Intn(4)],
			int64(30+rng.Intn(120))))
	}
	for i := range lots {
		lots[i].ID = seqUUID16(0x10, i)
		lots[i].InvoiceID = seqUUID16(0x20, i)
		lots[i].AssetID = seqUUID16(0x30, i)
	}

	a, err := auction.NewAuction(auction.NewAuctionParams{
		ID: uuid.MustParse("11111111-1111-4111-8111-111111111111"), IssuerID: issuers[0],
		Lots: lots, OpensAt: testOpens, ClosesAt: testClose,
	}, testNow)
	require.NoError(tb, err)

	bids := make([]*auction.Bid, 0, bidCount)
	for i := range bidCount {
		bid, err := auction.NewBid(auction.NewBidParams{
			ID: seqUUID16(0x40, i), AuctionID: a.ID, InvestorID: seqUUID16(0x50, i%200),
			Budget:         money.MustParse(fmt.Sprintf("%d.00", 2000+rng.Intn(30)*1000), money.USD),
			MinYield:       money.MustParseRate(fmt.Sprintf("0.0%d", 1+rng.Intn(8))),
			MaxGrade:       []risk.Grade{risk.GradeB, risk.GradeC, risk.GradeD}[rng.Intn(3)],
			MaxTenorDays:   int64(60 + rng.Intn(120)),
			MinimumLot:     money.MustParse(fmt.Sprintf("%d.00", rng.Intn(3)*500), money.USD),
			MaxIssuerShare: money.MustParseRate("0.35"),
			MaxDebtorShare: money.MustParseRate("0.25"),
		}, testNow)
		require.NoError(tb, err)
		bids = append(bids, bid)
	}
	return a, bids
}

// seqUUID16 spreads a sequence number over two bytes so a batch can exceed 256 entries.
func seqUUID16(prefix byte, seq int) uuid.UUID {
	var id uuid.UUID
	id[0] = prefix
	id[14] = byte(seq >> 8)
	id[15] = byte(seq)
	id[6] = 0x40 | (id[6] & 0x0f)
	id[8] = 0x80 | (id[8] & 0x3f)
	return id
}

// BenchmarkClearTargetBatch measures the batch size the specification targets: 500
// invoices and 2000 bids, to be cleared in under two seconds. The current solver does not
// meet that target; the benchmark exists to measure the gap rather than to hide it.
func BenchmarkClearTargetBatch(b *testing.B) {
	if testing.Short() {
		b.Skip("the target batch takes far longer than a short run allows")
	}

	a, bids := benchmarkBatch(b, 500, 2000)
	solver := auction.NewSolver()

	b.ResetTimer()
	for range b.N {
		solution, err := solver.Clear(a, bids, testClose)
		if err != nil {
			b.Fatal(err)
		}
		if len(solution.Allocations) == 0 {
			b.Fatal("expected a non-trivial allocation")
		}
	}
}

func BenchmarkClearSmallBatch(b *testing.B) {
	a, bids := benchmarkBatch(b, 50, 200)
	solver := auction.NewSolver()

	b.ResetTimer()
	for range b.N {
		if _, err := solver.Clear(a, bids, testClose); err != nil {
			b.Fatal(err)
		}
	}
}
