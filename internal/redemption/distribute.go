package redemption

import (
	"errors"
	"math/big"
	"sort"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// Distribute divides what the debtor paid among the parties that hold the receivable, in
// proportion to the notional each holds.
//
// Proportional division in whole minor units almost never comes out even, so the honest
// question is not whether there is a remainder but who gets it. Here it goes to the
// largest fractional parts first, ties broken by party id: every unit paid is handed to
// somebody, none is conjured, and the same payment splits the same way on every machine
// that computes it — which matters, because two parties read this result and only one of
// them is the platform.
//
// The arithmetic runs in big integers rather than int64. The intermediate product is
// amount × notional, which is not the size of anything real but is easily larger than
// either, and a silent overflow in a division of money is not a class of bug worth
// leaving open.
func Distribute(received money.Amount, holders []Holder) ([]Share, error) {
	if !received.IsValid() || !received.IsPositive() {
		return nil, apperr.Invalid("received", "must be a positive amount")
	}
	if len(holders) == 0 {
		return nil, apperr.Invalid("holders", "must name at least one holder")
	}

	var violations []error
	seen := make(map[uuid.UUID]struct{}, len(holders))
	total := new(big.Int)

	for i, holder := range holders {
		switch {
		case holder.PartyID == uuid.Nil:
			violations = append(violations, apperr.Invalid("holders", "holder %d has no party", i))
		case !holder.Notional.IsValid() || !holder.Notional.IsPositive():
			violations = append(violations, apperr.Invalid("holders",
				"holder %s holds no notional", holder.PartyID))
		case holder.Notional.Currency() != received.Currency():
			violations = append(violations, apperr.Invalid("holders",
				"holder %s holds %s against a payment in %s",
				holder.PartyID, holder.Notional.Currency(), received.Currency()))
		default:
			if _, duplicate := seen[holder.PartyID]; duplicate {
				// Two rows for one party would each get a share of the whole, which is how
				// a holder is quietly paid twice. The caller aggregates first.
				violations = append(violations, apperr.Invalid("holders",
					"party %s appears twice", holder.PartyID))
				continue
			}
			seen[holder.PartyID] = struct{}{}
			total.Add(total, big.NewInt(holder.Notional.Minor()))
		}
	}
	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	amount := big.NewInt(received.Minor())
	shares := make([]Share, len(holders))
	remainders := make([]*big.Int, len(holders))
	handedOut := new(big.Int)

	for i, holder := range holders {
		product := new(big.Int).Mul(amount, big.NewInt(holder.Notional.Minor()))
		whole, rest := new(big.Int).QuoRem(product, total, new(big.Int))

		part, err := money.New(whole.Int64(), received.Currency())
		if err != nil {
			return nil, err
		}
		shares[i] = Share{PartyID: holder.PartyID, Notional: holder.Notional, Amount: part}
		remainders[i] = rest
		handedOut.Add(handedOut, whole)
	}

	// What is left is strictly fewer units than there are holders, so one extra unit each
	// is always enough to finish.
	left := new(big.Int).Sub(amount, handedOut).Int64()

	order := make([]int, len(holders))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		i, j := order[a], order[b]
		if cmp := remainders[j].Cmp(remainders[i]); cmp != 0 {
			return cmp < 0 // the larger remainder first
		}
		// A tie is broken by party id rather than by input order, so a caller that lists
		// the same holders differently still gets the same division.
		return shares[i].PartyID.String() < shares[j].PartyID.String()
	})

	for _, index := range order[:left] {
		bumped, err := shares[index].Amount.Add(money.MustNew(1, received.Currency()))
		if err != nil {
			return nil, err
		}
		shares[index].Amount = bumped
	}

	return shares, nil
}
