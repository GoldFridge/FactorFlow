// Package agents holds the machine-facing endpoints: what a program, rather than a
// person, can buy from the platform.
//
// The two answers sold here are the two a counterparty actually needs before committing
// money: what a receivable is worth, and which lots in a batch fit a mandate. Both are
// computed from the published model and the stored market, never from anything the caller
// asserts, so an agent that pays twice for the same question gets the same answer.
package agents

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/auction"
	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// TxRunner is the read boundary the service needs.
type TxRunner interface {
	Querier() postgres.Querier
}

// Service answers the paid questions.
type Service struct {
	db        TxRunner
	snapshots marketdata.Repository
	// markets takes a new observation when the stored one has gone stale. It is optional:
	// without it this service can only sell prices somebody else's work kept fresh.
	markets *marketdata.Service
	auctions  auction.Repository
	model     risk.Model
	market    marketdata.Query
	now       func() time.Time
}

// Config wires the service.
type Config struct {
	DB        TxRunner
	Snapshots marketdata.Repository
	Markets   *marketdata.Service
	Auctions  auction.Repository
	Model     risk.Model
	Market    marketdata.Query
	Now       func() time.Time
}

// NewService returns the service.
func NewService(cfg Config) *Service {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Model.Version == "" {
		cfg.Model = risk.ModelV1()
	}
	return &Service{
		db:        cfg.DB,
		snapshots: cfg.Snapshots,
		markets:   cfg.Markets,
		auctions:  cfg.Auctions,
		model:     cfg.Model,
		market:    cfg.Market,
		now:       cfg.Now,
	}
}

/*
snapshot is the market this quote is priced against.

The stored one is used while it is fresh, and a new observation is taken when it is not.
That matters more here than anywhere else in the system: a machine customer pays before it
finds out whether an answer exists, so refusing it because nobody happened to have priced a
receivable in the last quarter of an hour would be charging for the platform's idleness.

Pricing still fails closed. If the market cannot be observed at all, no quote is produced —
a stale benchmark is wrong in a way the buyer cannot see, and this endpoint sells exactly
the thing that would be wrong.
*/
func (s *Service) snapshot(ctx context.Context) (*marketdata.Snapshot, error) {
	stored, err := s.snapshots.Latest(ctx, s.db.Querier(), s.market.Network, s.market.Asset)
	if err == nil && stored.IsFresh(s.now()) {
		return stored, nil
	}
	if err != nil && !apperr.IsNotFound(err) {
		return nil, err
	}
	if s.markets == nil {
		// Nothing can be observed, so the freshness rule is what stands: the stored snapshot
		// is either usable or this endpoint has nothing honest to sell.
		if stored == nil {
			return nil, apperr.Unavailablef("no market snapshot has been taken yet")
		}
		return stored, stored.EnsureFresh(s.now())
	}

	taken, err := s.markets.Snapshot(ctx, s.market)
	if err != nil {
		return nil, err
	}
	if err := taken.EnsureFresh(s.now()); err != nil {
		return nil, err
	}
	return taken, nil
}

// quoteRequest is what an agent asks for a price on.
//
// It carries the facts about a receivable rather than an invoice id: the point of the paid
// endpoint is that a counterparty can price a receivable the platform has never seen,
// without uploading a document to it.
type quoteRequest struct {
	Face      string `json:"face"`
	Currency  string `json:"currency"`
	DaysToDue int64  `json:"days_to_due"`

	// Features are the normalized signals the published model consumes. They are supplied
	// rather than derived because the caller is the one who knows its own debtor.
	Features struct {
		DSONorm             string `json:"dso_norm"`
		LatePaymentRate     string `json:"late_payment_rate"`
		DisputeFlag         string `json:"dispute_flag"`
		DebtorConcentration string `json:"debtor_concentration"`
		DebtorRisk          string `json:"debtor_risk"`
	} `json:"features"`

	Mitigations struct {
		Recourse       bool `json:"recourse"`
		Collateralized bool `json:"collateralized"`
	} `json:"mitigations"`
}

// quoteResponse is the answer an agent buys.
type quoteResponse struct {
	ModelVersion string `json:"model_version"`

	Grade string `json:"grade"`
	PD    string `json:"pd"`
	LGD   string `json:"lgd"`

	ExpectedLoss string `json:"expected_loss"`
	ReservePrice string `json:"reserve_price"`
	PlatformFee  string `json:"platform_fee"`
	Currency     string `json:"currency"`

	BenchmarkAPR string `json:"benchmark_apr"`
	DiscountAPR  string `json:"discount_apr"`
	Premiums     struct {
		Risk          string `json:"risk"`
		Liquidity     string `json:"liquidity"`
		Concentration string `json:"concentration"`
	} `json:"premiums"`

	// MarketSnapshotHash names the market this price was computed against, so the agent
	// can check the quote against the same public snapshot later.
	MarketSnapshotHash string `json:"market_snapshot_hash"`
	MarketObservedAt   string `json:"market_observed_at"`
	QuotedAt           string `json:"quoted_at"`
}

// RiskQuote prices a receivable the platform has never seen.
//
// The market half of the price comes from the stored snapshot rather than the request:
// letting a caller supply its own benchmark would let it quote itself any price it liked
// and then present the answer as the platform's.
func (s *Service) RiskQuote(ctx context.Context, body []byte) ([]byte, error) {
	var request quoteRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, apperr.Invalid("body", "must be a risk quote request")
	}

	currency, err := money.ParseCurrency(defaultString(request.Currency, money.USD.String()))
	if err != nil {
		return nil, apperr.Invalid("currency", "must be a supported currency")
	}
	face, err := money.Parse(request.Face, currency)
	if err != nil {
		return nil, apperr.Invalid("face", "must be a decimal amount")
	}
	if !face.IsPositive() {
		return nil, apperr.Invalid("face", "must be positive")
	}
	if request.DaysToDue <= 0 {
		return nil, apperr.Invalid("days_to_due", "must be positive")
	}

	features, err := parseFeatures(request)
	if err != nil {
		return nil, err
	}

	snapshot, err := s.snapshot(ctx)
	if err != nil {
		return nil, err
	}

	features.MarketVolatility = snapshot.Volatility

	pd, err := s.model.ProbabilityOfDefault(features)
	if err != nil {
		return nil, err
	}
	lgd := s.model.LossGivenDefault(risk.Mitigations{
		Recourse:       request.Mitigations.Recourse,
		Collateralized: request.Mitigations.Collateralized,
	})
	expectedLoss, err := s.model.ExpectedLoss(pd, lgd, face)
	if err != nil {
		return nil, err
	}

	price, err := s.model.Price(risk.PriceInput{
		Face:                face,
		DaysToDue:           request.DaysToDue,
		Benchmark:           snapshot.Benchmark,
		LiquidityPremium:    snapshot.LiquidityPremium,
		PD:                  pd,
		LGD:                 lgd,
		DebtorConcentration: features.DebtorConcentration,
	})
	if err != nil {
		return nil, err
	}

	response := quoteResponse{
		ModelVersion:       s.model.Version,
		Grade:              risk.GradeFromPD(pd).String(),
		PD:                 pd.String(),
		LGD:                lgd.String(),
		ExpectedLoss:       expectedLoss.String(),
		ReservePrice:       price.ReservePrice.String(),
		PlatformFee:        price.PlatformFee.String(),
		Currency:           currency.String(),
		BenchmarkAPR:       snapshot.Benchmark.String(),
		DiscountAPR:        price.DiscountAPR.String(),
		MarketSnapshotHash: snapshot.PayloadHash,
		MarketObservedAt:   snapshot.ObservedAt.Format(time.RFC3339),
		QuotedAt:           s.now().UTC().Format(time.RFC3339),
	}
	response.Premiums.Risk = price.Premiums.Risk.String()
	response.Premiums.Liquidity = price.Premiums.Liquidity.String()
	response.Premiums.Concentration = price.Premiums.Concentration.String()

	return json.Marshal(response)
}

// recommendationRequest is an investor agent's mandate.
type recommendationRequest struct {
	AuctionID    string `json:"auction_id"`
	Budget       string `json:"budget"`
	Currency     string `json:"currency"`
	MinYield     string `json:"min_yield"`
	MaxGrade     string `json:"max_grade"`
	MaxTenorDays int64  `json:"max_tenor_days"`
}

// recommendedLot is one lot that fits, with the numbers behind why.
type recommendedLot struct {
	LotID        string `json:"lot_id"`
	DebtorRef    string `json:"debtor_ref"`
	Grade        string `json:"grade"`
	TenorDays    int64  `json:"tenor_days"`
	Supply       string `json:"supply"`
	ReservePrice string `json:"reserve_price"`
	ImpliedYield string `json:"implied_yield"`
}

// rejectedLot names a lot that does not fit and which limit refused it, so an agent can
// adjust its mandate rather than guess.
type rejectedLot struct {
	LotID      string `json:"lot_id"`
	Constraint string `json:"constraint"`
}

type recommendationResponse struct {
	AuctionID string `json:"auction_id"`
	Status    string `json:"status"`
	ClosesAt  string `json:"closes_at"`

	Eligible []recommendedLot `json:"eligible"`
	Rejected []rejectedLot    `json:"rejected"`

	// SuggestedBudget is what taking every eligible lot at its reserve price would cost.
	SuggestedBudget string `json:"suggested_budget"`
	Currency        string `json:"currency"`
	// BudgetShortfall is how much more the mandate would need to take all of them, zero
	// when the budget already covers it.
	BudgetShortfall string `json:"budget_shortfall"`

	RecommendedAt string `json:"recommended_at"`
}

// AuctionRecommendation reports which lots in a batch fit a mandate.
//
// It applies the same constraints the solver applies, so an agent that bids on this advice
// is not told one thing and cleared by another. What it does not do is decide: the numbers
// are published and the agent chooses.
func (s *Service) AuctionRecommendation(ctx context.Context, body []byte) ([]byte, error) {
	var request recommendationRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, apperr.Invalid("body", "must be a recommendation request")
	}

	auctionID, err := uuid.Parse(request.AuctionID)
	if err != nil {
		return nil, apperr.Invalid("auction_id", "must be a UUID")
	}

	currency, err := money.ParseCurrency(defaultString(request.Currency, money.USD.String()))
	if err != nil {
		return nil, apperr.Invalid("currency", "must be a supported currency")
	}
	budget, err := money.Parse(defaultString(request.Budget, "0"), currency)
	if err != nil {
		return nil, apperr.Invalid("budget", "must be a decimal amount")
	}
	minYield, err := money.ParseRate(defaultString(request.MinYield, "0"))
	if err != nil {
		return nil, apperr.Invalid("min_yield", "must be a decimal rate")
	}
	maxGrade, err := risk.ParseGrade(defaultString(request.MaxGrade, risk.GradeE.String()))
	if err != nil {
		return nil, apperr.Invalid("max_grade", "must be one of A, B, C, D, E")
	}

	a, err := s.auctions.GetAuction(ctx, s.db.Querier(), auctionID)
	if err != nil {
		return nil, err
	}

	response := recommendationResponse{
		AuctionID:     a.ID.String(),
		Status:        a.Status.String(),
		ClosesAt:      a.ClosesAt.Format(time.RFC3339),
		Currency:      currency.String(),
		RecommendedAt: s.now().UTC().Format(time.RFC3339),
		Eligible:      []recommendedLot{},
		Rejected:      []rejectedLot{},
	}

	total := money.Zero(currency)
	for _, lot := range a.Lots {
		yield, err := lot.ImpliedYield()
		if err != nil {
			return nil, err
		}

		if constraint, ok := refusedBy(lot, yield, minYield, maxGrade, request.MaxTenorDays, currency); ok {
			response.Rejected = append(response.Rejected, rejectedLot{
				LotID: lot.ID.String(), Constraint: constraint,
			})
			continue
		}

		response.Eligible = append(response.Eligible, recommendedLot{
			LotID:        lot.ID.String(),
			DebtorRef:    lot.DebtorRef,
			Grade:        lot.Grade.String(),
			TenorDays:    lot.TenorDays,
			Supply:       lot.Supply.String(),
			ReservePrice: lot.ReservePrice.String(),
			ImpliedYield: yield.StringFixed(6),
		})

		if total, err = total.Add(lot.ReservePrice); err != nil {
			return nil, err
		}
	}

	response.SuggestedBudget = total.String()

	shortfall := money.Zero(currency)
	if budget.IsValid() {
		if cmp, err := total.Cmp(budget); err == nil && cmp > 0 {
			if shortfall, err = total.Sub(budget); err != nil {
				return nil, err
			}
		}
	}
	response.BudgetShortfall = shortfall.String()

	return json.Marshal(response)
}

// refusedBy reports the first constraint a lot violates, named the way the solver names it
// when it rejects a bid, so the two vocabularies agree.
func refusedBy(lot auction.Lot, yield, minYield money.Rate, maxGrade risk.Grade, maxTenorDays int64, currency money.Currency) (string, bool) {
	switch {
	case lot.Supply.Currency() != currency:
		return string(auction.ConstraintCurrency), true
	case yield.Cmp(minYield) < 0:
		return string(auction.ConstraintYield), true
	case !lot.Grade.AtMost(maxGrade):
		return string(auction.ConstraintGrade), true
	case maxTenorDays > 0 && lot.TenorDays > maxTenorDays:
		return string(auction.ConstraintMaturity), true
	}
	return "", false
}

func parseFeatures(request quoteRequest) (risk.FeatureVector, error) {
	fields := []struct {
		name  string
		value string
		set   func(*risk.FeatureVector, money.Rate)
	}{
		{"dso_norm", request.Features.DSONorm, func(f *risk.FeatureVector, r money.Rate) { f.DSONorm = r }},
		{"late_payment_rate", request.Features.LatePaymentRate, func(f *risk.FeatureVector, r money.Rate) { f.LatePaymentRate = r }},
		{"dispute_flag", request.Features.DisputeFlag, func(f *risk.FeatureVector, r money.Rate) { f.DisputeFlag = r }},
		{"debtor_concentration", request.Features.DebtorConcentration, func(f *risk.FeatureVector, r money.Rate) { f.DebtorConcentration = r }},
		{"debtor_risk", request.Features.DebtorRisk, func(f *risk.FeatureVector, r money.Rate) { f.DebtorRisk = r }},
	}

	var features risk.FeatureVector
	for _, field := range fields {
		rate, err := money.ParseRate(defaultString(field.value, "0"))
		if err != nil {
			return features, apperr.Invalid("features."+field.name, "must be a decimal rate")
		}
		field.set(&features, rate)
	}

	// Validation happens against the published range, not a private one: a caller sending
	// a feature outside [0,1] has a different idea of normalization, and scoring it anyway
	// would produce a number neither side could defend.
	features.MarketVolatility = money.ZeroRate()
	if err := features.Validate(); err != nil {
		return features, err
	}
	return features, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// The paths the paid endpoints are published at. They are constants because an agent was
// given these strings and the payment it makes is bound to one of them.
const (
	RouteRiskQuote      = "/paid/v1/risk-quote"
	RouteRecommendation = "/paid/v1/auction-recommendation"
)
