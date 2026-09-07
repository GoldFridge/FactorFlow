package auction

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/GoldFridge/factorflow/internal/risk"
)

// certificateFormat identifies the canonical serialization. Changing the format changes
// every hash, so it is versioned explicitly rather than left implicit in the code.
const certificateFormat = "factorflow-allocation-certificate/v1"

// Field and record separators, chosen as control characters that cannot appear in an id,
// a decimal literal or a debtor reference.
const (
	certFieldSep  = "\x1f"
	certRecordSep = "\x1e"
)

// Certificate is the canonical hash of a clearing: its inputs, the solver and its
// published parameters, and the resulting allocation.
//
// Rerunning the same solver on the same canonical inputs must reproduce this hash exactly.
// That is what a judge checks: not that the allocation looks reasonable, but that it is
// the one this input could only have produced.
func Certificate(a *Auction, bids []*Bid, s *Solution, params SolverParams) string {
	var b strings.Builder

	b.WriteString(certificateFormat)
	b.WriteString(certFieldSep)
	b.WriteString(a.ID.String())
	b.WriteString(certFieldSep)
	b.WriteString(s.SolverVersion)
	writeParams(&b, params)

	lots := append([]Lot(nil), a.Lots...)
	sort.Slice(lots, func(i, j int) bool {
		if lots[i].InvoiceID != lots[j].InvoiceID {
			return lots[i].InvoiceID.String() < lots[j].InvoiceID.String()
		}
		return lots[i].ID.String() < lots[j].ID.String()
	})
	for _, lot := range lots {
		b.WriteString(certRecordSep)
		b.WriteString("lot")
		writeFields(&b,
			lot.ID.String(), lot.InvoiceID.String(), lot.AssetID.String(),
			lot.IssuerID.String(), lot.DebtorRef,
			strconv.FormatInt(lot.Supply.Minor(), 10),
			strconv.FormatInt(lot.ReservePrice.Minor(), 10),
			lot.Supply.Currency().String(),
			lot.Grade.String(),
			strconv.FormatInt(lot.TenorDays, 10),
		)
	}

	sorted := append([]*Bid(nil), bids...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].CreatedAt.Equal(sorted[j].CreatedAt) {
			return sorted[i].CreatedAt.Before(sorted[j].CreatedAt)
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	for _, bid := range sorted {
		b.WriteString(certRecordSep)
		b.WriteString("bid")
		writeFields(&b,
			bid.ID.String(), bid.InvestorID.String(),
			strconv.FormatInt(bid.Budget.Minor(), 10),
			bid.Budget.Currency().String(),
			bid.MinYield.StringFixed(rateScale),
			bid.MaxGrade.String(),
			strconv.FormatInt(bid.MaxTenorDays, 10),
			strconv.FormatInt(bid.MinimumLot.Minor(), 10),
			bid.MaxIssuerShare.StringFixed(rateScale),
			bid.MaxDebtorShare.StringFixed(rateScale),
			gradeShares(bid),
			bid.Status.String(),
		)
	}

	for _, allocation := range s.Allocations {
		b.WriteString(certRecordSep)
		b.WriteString("allocation")
		writeFields(&b,
			strconv.Itoa(allocation.Rank),
			allocation.LotID.String(), allocation.BidID.String(),
			strconv.FormatInt(allocation.Notional.Minor(), 10),
			strconv.FormatInt(allocation.Price.Minor(), 10),
		)
	}

	rejections := append([]Rejection(nil), s.Rejections...)
	sort.Slice(rejections, func(i, j int) bool {
		return rejections[i].BidID.String() < rejections[j].BidID.String()
	})
	for _, rejection := range rejections {
		b.WriteString(certRecordSep)
		b.WriteString("rejection")
		writeFields(&b, rejection.BidID.String(), rejection.LotID.String(), rejection.Constraint.String())
	}

	b.WriteString(certRecordSep)
	b.WriteString("result")
	writeFields(&b,
		strconv.FormatInt(s.Objective, 10),
		strconv.FormatInt(s.TotalNotional.Minor(), 10),
		strconv.FormatInt(s.TotalCash.Minor(), 10),
	)

	sum := sha256.Sum256([]byte(b.String()))
	return "0x" + hex.EncodeToString(sum[:])
}

func writeParams(b *strings.Builder, params SolverParams) {
	writeFields(b,
		params.SurplusWeight.StringFixed(rateScale),
		params.FundingWeight.StringFixed(rateScale),
		params.RiskPenalty.StringFixed(rateScale),
		strconv.Itoa(params.MaxAugmentations),
		strconv.Itoa(params.MaxBranchNodes),
		strconv.Itoa(params.MaxRepairRounds),
	)
}

func writeFields(b *strings.Builder, fields ...string) {
	for _, field := range fields {
		b.WriteString(certFieldSep)
		b.WriteString(field)
	}
}

// gradeShares renders a bid's per-grade limits in grade order, so an unordered map cannot
// change a certificate hash.
func gradeShares(bid *Bid) string {
	if len(bid.MaxGradeShare) == 0 {
		return ""
	}
	grades := make([]risk.Grade, 0, len(bid.MaxGradeShare))
	for grade := range bid.MaxGradeShare {
		grades = append(grades, grade)
	}
	sort.Slice(grades, func(i, j int) bool { return grades[i].String() < grades[j].String() })

	parts := make([]string, 0, len(grades))
	for _, grade := range grades {
		parts = append(parts, grade.String()+"="+bid.MaxGradeShare[grade].StringFixed(rateScale))
	}
	return strings.Join(parts, ",")
}
