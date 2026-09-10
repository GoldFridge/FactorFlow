package assessment

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoldFridge/factorflow/internal/platform/llm"
	"github.com/GoldFridge/factorflow/internal/risk"
)

/*
Narrator turns a published assessment into words.

The division of labour is the specification's, and it is the reason this is worth building
at all: the model is told the numbers and asked which of them mattered, never asked what
anything should cost. It cannot reach the price, because by the time it is called the price
is stored, and it cannot quote a figure the assessment did not publish, because what it
writes is checked against exactly that set before it is kept.

What the model is given is also the whole of what it may see. There is no document text in
the prompt, no invoice number, no debtor and no party: a feature vector, six weighted
contributions, and the prices those produced. The privacy rule is not enforced by asking
the model nicely — the content simply is not there.
*/
type Narrator struct {
	client *llm.Client
	now    func() time.Time
}

// NewNarrator returns a narrator. A nil client is normal: without one, every assessment is
// explained by the derived narration, which is the same words the check would have demanded.
func NewNarrator(client *llm.Client, now func() time.Time) *Narrator {
	if now == nil {
		now = time.Now
	}
	return &Narrator{client: client, now: now}
}

// Available reports whether a model is configured at all.
func (n *Narrator) Available() bool { return n != nil && n.client != nil }

const system = `You explain credit decisions that have already been made.

You will be given the published output of a deterministic risk model: a feature vector, the
weighted contribution of each feature, and the prices those produced. Write at most four
short bullet points, one sentence each, saying what drove this score and what the price is
built from. Address a professional investor deciding whether to bid.

Rules you must follow:
- Use only numbers that appear in the input. Never compute, round or estimate a new one.
- Never say what the price should be, whether it is fair, or whether to buy.
- Do not speculate about the debtor, the issuer or the document. You have not seen them.
- Write plain English. Name a quantity the way a person would — the default probability, the
  market benchmark, the reserve price — never by its field name, and never in snake_case.
- No preamble, no closing, no markdown headings. One bullet per line, starting with "- ".`

/*
Narrate asks the model and keeps what it wrote only if it survives checking.

Every failure ends the same way: the derived explanation, which says what the stored numbers
say and nothing else. That is deliberate. A narration is a convenience attached to a price
that was already decided, and there is no failure of a convenience that should leave a
reader with no explanation at all — or, worse, waiting for one.
*/
func (n *Narrator) Narrate(ctx context.Context, assessment *risk.Assessment) *risk.Explanation {
	derived := risk.Derive(assessment, n.now())
	if !n.Available() || assessment == nil {
		return derived
	}

	answer, err := n.client.Complete(ctx, []llm.Message{
		{Role: llm.RoleSystem, Content: system},
		{Role: llm.RoleUser, Content: prompt(assessment)},
	}, 400)
	if err != nil {
		slog.InfoContext(ctx, "the model did not answer; explaining from the contributions",
			slog.String("assessment_id", assessment.ID.String()),
			slog.String("error", err.Error()))
		return derived
	}

	explanation, err := risk.Narrate(assessment, n.client.Model(), bullets(answer), n.now())
	if err != nil {
		// What it wrote broke the one rule that matters, so none of it is kept. Salvaging
		// the lines that happened to pass would publish a narration nobody wrote.
		//
		// The refusal is logged rather than swallowed: a model that is rejected every time
		// is a prompt that needs fixing, and nothing else in the system would show it. The
		// text is safe to log — it is words about six coefficients and a price, and the
		// prompt it answered contained no document.
		slog.InfoContext(ctx, "the model's explanation was refused; explaining from the contributions",
			slog.String("assessment_id", assessment.ID.String()),
			slog.String("reason", err.Error()),
			slog.String("answer", answer))
		return derived
	}
	return explanation
}

// prompt is the whole of what the model is shown.
func prompt(a *risk.Assessment) string {
	var b strings.Builder

	fmt.Fprintf(&b, "model_version: %s\n", a.ModelVersion)
	fmt.Fprintf(&b, "grade: %s\n", a.Grade)
	fmt.Fprintf(&b, "probability_of_default: %s\n", risk.Percent(a.PD))
	fmt.Fprintf(&b, "loss_given_default: %s\n", risk.Percent(a.LGD))
	fmt.Fprintf(&b, "extraction_confidence: %s\n", risk.Percent(a.Confidence))
	fmt.Fprintf(&b, "expected_loss: %s\n", a.ExpectedLoss)
	fmt.Fprintf(&b, "platform_fee: %s\n", a.PlatformFee)
	fmt.Fprintf(&b, "reserve_price: %s\n", a.ReservePrice)
	fmt.Fprintf(&b, "market_benchmark_apr: %s\n", risk.Percent(a.BenchmarkAPR))
	fmt.Fprintf(&b, "risk_premium: %s\n", risk.Percent(a.Premiums.Risk))
	fmt.Fprintf(&b, "liquidity_premium: %s\n", risk.Percent(a.Premiums.Liquidity))
	fmt.Fprintf(&b, "concentration_premium: %s\n", risk.Percent(a.Premiums.Concentration))
	fmt.Fprintf(&b, "discount_apr: %s\n", risk.Percent(a.DiscountAPR))

	b.WriteString("\ncontributions, largest first (feature, value, weight, effect on log-odds):\n")
	for _, c := range a.RankedContributions() {
		fmt.Fprintf(&b, "- %s: value %s, weight %s, effect %s\n",
			c.Feature, c.Value, c.Weight, c.Effect)
	}

	if a.RequiresManualReview {
		b.WriteString("\nThis assessment is held for manual review: the extraction confidence " +
			"gate was not met.\n")
	}
	return b.String()
}

// bullets splits an answer into points, tolerating the shapes a model actually returns.
func bullets(answer string) []string {
	lines := strings.Split(answer, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
