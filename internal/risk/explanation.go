package risk

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// Explanation is words about a price that was already decided.
//
// The separation is the point of the whole module. A language model is good at saying which
// of six weighted terms mattered and bad at arithmetic nobody can check, so it is allowed
// to narrate an assessment and never to produce one. This type is the narration, stored
// beside the assessment rather than inside it: the priced record is immutable, and words
// about it can arrive late, fail to arrive, or be replaced.
type Explanation struct {
	AssessmentID uuid.UUID
	// Source is who wrote it: a model, or this package from the stored contributions.
	Source string
	// Model names the version that wrote it, empty for a derived explanation.
	Model     string
	Bullets   []string
	CreatedAt time.Time
}

// The sources an explanation can come from.
const (
	// SourceModel is a language model's narration, checked before it is kept.
	SourceModel = "model"
	// SourceDerived is this package's own, built from the ranked contributions. It is what
	// a reader sees when no model answered or when what it wrote failed the check.
	SourceDerived = "derived"
)

// Limits on what a narration may be.
const (
	MaxBullets   = 5
	MaxBulletLen = 200
)

// numbers finds anything a reader would take as a figure: 60, 3.63, 1,250.00, 12%.
var numbers = regexp.MustCompile(`\d[\d,]*(?:\.\d+)?`)

/*
Narrate keeps a model's words only if every number in them is one the assessment published.

This is the control that makes the separation real rather than declared. A narration that
may invent figures is a second, unaccountable pricing path: nobody reading "the discount is
about nine percent" beside a published 14.81% knows which to believe, and the one that
sounds like prose is the one people repeat. So a number the model did not get from the
assessment is not a wording problem to be tidied up — it is grounds to discard the whole
narration and use the derived one instead.

Everything else it says is its own: which driver dominated, what a grade means, why a short
tenor matters. That is the part a model is actually better at.
*/
func Narrate(a *Assessment, model string, bullets []string, now time.Time) (*Explanation, error) {
	if a == nil {
		return nil, apperr.Invalid("assessment", "must not be nil")
	}

	cleaned := make([]string, 0, len(bullets))
	for _, bullet := range bullets {
		trimmed := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(bullet), "-•*"))
		trimmed = strings.TrimSpace(trimmed)
		if trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}

	var violations []error
	switch {
	case len(cleaned) == 0:
		violations = append(violations, apperr.Invalid("bullets", "must not be empty"))
	case len(cleaned) > MaxBullets:
		violations = append(violations, apperr.Invalid("bullets",
			"must be at most %d points", MaxBullets))
	}

	allowed := publishedNumbers(a)
	for i, bullet := range cleaned {
		if len(bullet) > MaxBulletLen {
			violations = append(violations, apperr.Invalid("bullets",
				"point %d is longer than %d characters", i+1, MaxBulletLen))
		}
		for _, figure := range numbers.FindAllString(bullet, -1) {
			if _, ok := allowed[strings.ReplaceAll(figure, ",", "")]; !ok {
				violations = append(violations, apperr.Invalid("bullets",
					"point %d cites %q, which this assessment did not publish", i+1, figure))
			}
		}
	}

	if err := errors.Join(violations...); err != nil {
		return nil, err
	}

	return &Explanation{
		AssessmentID: a.ID,
		Source:       SourceModel,
		Model:        strings.TrimSpace(model),
		Bullets:      cleaned,
		CreatedAt:    now.UTC(),
	}, nil
}

/*
Derive writes the explanation this package can defend on its own.

It exists so that a reader always has words, and so that a model has something to be
compared against. It says only what the stored numbers say: which terms moved the score, in
which direction, and what the price was built from.
*/
func Derive(a *Assessment, now time.Time) *Explanation {
	if a == nil {
		return nil
	}

	ranked := a.RankedContributions()
	bullets := make([]string, 0, MaxBullets)

	if len(ranked) > 0 {
		bullets = append(bullets, fmt.Sprintf("%s moved the score most, %s it.",
			words(ranked[0].Feature), direction(ranked[0].Effect)))
	}
	if len(ranked) > 1 {
		bullets = append(bullets, fmt.Sprintf("%s came next, %s it.",
			words(ranked[1].Feature), direction(ranked[1].Effect)))
	}

	bullets = append(bullets, fmt.Sprintf(
		"Grade %s: a default probability of %s against a loss given default of %s.",
		a.Grade, Percent(a.PD), Percent(a.LGD)))
	bullets = append(bullets, fmt.Sprintf(
		"The discount of %s is the market benchmark of %s plus %s of premiums.",
		Percent(a.DiscountAPR), Percent(a.BenchmarkAPR), Percent(a.Premiums.Total())))
	bullets = append(bullets, fmt.Sprintf(
		"After an expected loss of %s and the platform fee, the reserve price is %s.",
		a.ExpectedLoss, a.ReservePrice))

	return &Explanation{
		AssessmentID: a.ID,
		Source:       SourceDerived,
		Bullets:      bullets,
		CreatedAt:    now.UTC(),
	}
}

/*
publishedNumbers is every figure a narration may use, in the shapes a writer would use them.

A rate lives in the assessment as 0.036325 and is spoken as 3.63%, so both are here, along
with the one-decimal rounding a sentence tends to reach for. Amounts appear with and without
their grouping. The set is generous about form and strict about origin: what it cannot
contain is a number that is not in this assessment.
*/
func publishedNumbers(a *Assessment) map[string]struct{} {
	allowed := map[string]struct{}{}

	add := func(value string) {
		value = strings.TrimSpace(strings.ReplaceAll(value, ",", ""))
		if value != "" {
			allowed[value] = struct{}{}
		}
	}

	addRate := func(r money.Rate) {
		add(r.String())
		add(r.StringFixed(2))
		add(r.StringFixed(4))
		hundred := r.Mul(money.RateFromInt(100))
		add(hundred.String())
		add(hundred.StringFixed(0))
		add(hundred.StringFixed(1))
		add(hundred.StringFixed(2))
		// Percentages are commonly written without a trailing zero: 3.6 rather than 3.60.
		add(strings.TrimRight(strings.TrimRight(hundred.StringFixed(2), "0"), "."))
	}

	addAmount := func(m money.Amount) {
		add(m.String())
		add(strings.TrimRight(strings.TrimRight(m.String(), "0"), "."))
	}

	addRate(a.PD)
	addRate(a.LGD)
	addRate(a.Confidence)
	addRate(a.BenchmarkAPR)
	addRate(a.DiscountAPR)
	addRate(a.Premiums.Risk)
	addRate(a.Premiums.Liquidity)
	addRate(a.Premiums.Concentration)
	addRate(a.Premiums.Total())

	addAmount(a.ExpectedLoss)
	addAmount(a.ReservePrice)
	addAmount(a.PlatformFee)

	for _, contribution := range a.Contributions {
		addRate(contribution.Value)
		addRate(contribution.Weight)
		addRate(contribution.Effect)
	}
	for name, value := range featureValues(a.Features) {
		_ = name
		addRate(value)
	}
	return allowed
}

// featureValues is the scored vector by name, so a narration may quote what it was given.
func featureValues(f FeatureVector) map[string]money.Rate {
	return map[string]money.Rate{
		"dso_norm":             f.DSONorm,
		"late_payment_rate":    f.LatePaymentRate,
		"dispute_flag":         f.DisputeFlag,
		"debtor_concentration": f.DebtorConcentration,
		"market_volatility":    f.MarketVolatility,
		"debtor_risk":          f.DebtorRisk,
	}
}

// direction says which way a term pushed, in words rather than a sign.
func direction(effect money.Rate) string {
	if effect.Decimal().IsNegative() {
		return "lowering"
	}
	return "raising"
}

// words turns a feature name into something a person reads.
func words(feature string) string {
	spaced := strings.ReplaceAll(feature, "_", " ")
	if spaced == "" {
		return spaced
	}
	return strings.ToUpper(spaced[:1]) + spaced[1:]
}

// Percent renders a rate the way the interface does, so a narration, a prompt and the
// screen all speak about the same number in the same shape.
func Percent(r money.Rate) string {
	return r.Mul(money.RateFromInt(100)).StringFixed(2) + "%"
}

// FeatureNamesSorted is the stable order a prompt lists features in, so two runs of the same
// assessment ask the same question.
func FeatureNamesSorted() []string {
	names := FeatureNames()
	sort.Strings(names)
	return names
}
