package reporting

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// Handler exposes the read-only explanations.
type Handler struct {
	service *Service
	now     func() time.Time
}

// NewHandler returns the handler.
func NewHandler(service *Service, now func() time.Time) *Handler {
	if now == nil {
		now = time.Now
	}
	return &Handler{service: service, now: now}
}

// Routes registers the endpoints.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/invoices/{invoiceID}/assessment", h.assessment)
	r.Get("/invoices/{invoiceID}/timeline", h.timeline)
	r.Get("/market/benchmarks/latest", h.benchmark)
}

// assessmentResponse is the wire shape of one price and its reasoning.
//
// Every rate is a decimal string, and every amount carries its currency beside it: a JSON
// number would round the values a participant is meant to be able to recompute.
type assessmentResponse struct {
	ID           string `json:"id"`
	InvoiceID    string `json:"invoice_id"`
	ModelVersion string `json:"model_version"`

	Grade      string `json:"grade"`
	PD         string `json:"pd"`
	LGD        string `json:"lgd"`
	Confidence string `json:"confidence"`

	ExpectedLoss string `json:"expected_loss"`
	ReservePrice string `json:"reserve_price"`
	PlatformFee  string `json:"platform_fee"`
	Currency     string `json:"currency"`

	BenchmarkAPR string           `json:"benchmark_apr"`
	DiscountAPR  string           `json:"discount_apr"`
	Premiums     premiumsResponse `json:"premiums"`

	Features      map[string]string      `json:"features"`
	Contributions []contributionResponse `json:"contributions"`

	MarketSnapshotHash     string `json:"market_snapshot_hash"`
	ConfidentialCommitment string `json:"confidential_commitment"`
	// ConfidentialNonce is published on purpose: the commitment is only checkable by
	// someone who can recompute it.
	ConfidentialNonce string `json:"confidential_nonce"`

	RequiresManualReview bool   `json:"requires_manual_review"`
	CreatedAt            string `json:"created_at"`

	// Snapshot is the market the benchmark was taken from, when it is still on record.
	Snapshot *snapshotResponse `json:"market_snapshot,omitempty"`
}

type premiumsResponse struct {
	Risk          string `json:"risk"`
	Liquidity     string `json:"liquidity"`
	Concentration string `json:"concentration"`
	Total         string `json:"total"`
}

// contributionResponse is one feature's share of the score, which is what an explanation
// is allowed to narrate.
type contributionResponse struct {
	Feature string `json:"feature"`
	Value   string `json:"value"`
	Weight  string `json:"weight"`
	Effect  string `json:"effect"`
}

type snapshotResponse struct {
	PayloadHash      string   `json:"payload_hash"`
	QueryHash        string   `json:"query_hash"`
	Provider         string   `json:"provider"`
	Network          string   `json:"network"`
	Asset            string   `json:"asset"`
	BenchmarkAPR     string   `json:"benchmark_apr"`
	LiquidityPremium string   `json:"liquidity_premium"`
	Volatility       string   `json:"volatility"`
	TotalLiquidity   string   `json:"total_liquidity"`
	Currency         string   `json:"currency"`
	MarketCount      int      `json:"market_count"`
	SubgraphIDs      []string `json:"subgraph_ids"`
	BlockNumbers     []int64  `json:"block_numbers"`
	ObservedAt       string   `json:"observed_at"`
	ExpiresAt        string   `json:"expires_at"`
	// Fresh reports whether a price may still be published from this snapshot.
	Fresh bool `json:"fresh"`
}

func (h *Handler) assessment(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "invoiceID"))
	if err != nil {
		httpserver.WriteProblem(w, r, apperr.Invalid("invoice_id", "must be a UUID"))
		return
	}

	report, err := h.service.AssessmentFor(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, h.toAssessmentResponse(report))
}

// eventResponse is one entry of the audit timeline.
type eventResponse struct {
	Actor      string         `json:"actor"`
	Action     string         `json:"action"`
	EntityType string         `json:"entity_type"`
	EntityID   string         `json:"entity_id"`
	BeforeHash string         `json:"before_hash,omitempty"`
	AfterHash  string         `json:"after_hash,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	Detail     map[string]any `json:"detail"`
	OccurredAt string         `json:"occurred_at"`
}

type timelineResponse struct {
	Items []eventResponse `json:"items"`
}

func (h *Handler) timeline(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "invoiceID"))
	if err != nil {
		httpserver.WriteProblem(w, r, apperr.Invalid("invoice_id", "must be a UUID"))
		return
	}

	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			httpserver.WriteProblem(w, r, apperr.Invalid("limit", "must be a positive integer"))
			return
		}
		limit = parsed
	}

	events, err := h.service.TimelineFor(r.Context(), actorOf(r), id, limit)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	items := make([]eventResponse, 0, len(events))
	for _, e := range events {
		detail := e.Detail
		if detail == nil {
			detail = map[string]any{}
		}
		items = append(items, eventResponse{
			Actor:      e.Actor,
			Action:     e.Action,
			EntityType: e.EntityType,
			EntityID:   e.EntityID,
			BeforeHash: e.BeforeHash,
			AfterHash:  e.AfterHash,
			TraceID:    e.TraceID,
			Detail:     detail,
			OccurredAt: e.OccurredAt.Format(time.RFC3339),
		})
	}
	httpserver.WriteJSON(w, r, http.StatusOK, timelineResponse{Items: items})
}

func (h *Handler) benchmark(w http.ResponseWriter, r *http.Request) {
	now := h.now()
	snapshot, fresh, err := h.service.LatestSnapshot(r.Context(), actorOf(r), now)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toSnapshotResponse(snapshot, fresh))
}

func (h *Handler) toAssessmentResponse(report *Report) assessmentResponse {
	a := report.Assessment

	contributions := make([]contributionResponse, 0, len(report.Contributions))
	for _, c := range report.Contributions {
		contributions = append(contributions, contributionResponse{
			Feature: c.Feature,
			Value:   c.Value.String(),
			Weight:  c.Weight.String(),
			Effect:  c.Effect.String(),
		})
	}

	out := assessmentResponse{
		ID:           a.ID.String(),
		InvoiceID:    a.InvoiceID.String(),
		ModelVersion: a.ModelVersion,

		Grade:      a.Grade.String(),
		PD:         a.PD.String(),
		LGD:        a.LGD.String(),
		Confidence: a.Confidence.String(),

		ExpectedLoss: a.ExpectedLoss.String(),
		ReservePrice: a.ReservePrice.String(),
		PlatformFee:  a.PlatformFee.String(),
		Currency:     a.ReservePrice.Currency().String(),

		BenchmarkAPR: a.BenchmarkAPR.String(),
		DiscountAPR:  a.DiscountAPR.String(),
		Premiums: premiumsResponse{
			Risk:          a.Premiums.Risk.String(),
			Liquidity:     a.Premiums.Liquidity.String(),
			Concentration: a.Premiums.Concentration.String(),
			Total:         a.Premiums.Total().String(),
		},

		Features:      featuresOf(a.Features),
		Contributions: contributions,

		MarketSnapshotHash:     a.MarketSnapshotHash,
		ConfidentialCommitment: a.ConfidentialCommitment,
		ConfidentialNonce:      a.ConfidentialNonce,

		RequiresManualReview: a.RequiresManualReview,
		CreatedAt:            a.CreatedAt.Format(time.RFC3339),
	}

	if report.Snapshot != nil {
		snapshot := toSnapshotResponse(report.Snapshot, report.Snapshot.IsFresh(h.now()))
		out.Snapshot = &snapshot
	}
	return out
}

func toSnapshotResponse(s *marketdata.Snapshot, fresh bool) snapshotResponse {
	return snapshotResponse{
		PayloadHash:      s.PayloadHash,
		QueryHash:        s.QueryHash,
		Provider:         s.Provider,
		Network:          s.Network,
		Asset:            s.Asset,
		BenchmarkAPR:     s.Benchmark.String(),
		LiquidityPremium: s.LiquidityPremium.String(),
		Volatility:       s.Volatility.String(),
		TotalLiquidity:   s.TotalLiquidity.String(),
		Currency:         s.TotalLiquidity.Currency().String(),
		MarketCount:      len(s.Markets),
		SubgraphIDs:      s.SubgraphIDs,
		BlockNumbers:     s.BlockNumbers,
		ObservedAt:       s.ObservedAt.Format(time.RFC3339),
		ExpiresAt:        s.ExpiresAt().Format(time.RFC3339),
		Fresh:            fresh,
	}
}

// featuresOf renders the scored feature vector by the same names the contributions use, so
// a reader can line the two up.
func featuresOf(f risk.FeatureVector) map[string]string {
	return map[string]string{
		"dso_norm":             f.DSONorm.String(),
		"late_payment_rate":    f.LatePaymentRate.String(),
		"dispute_flag":         f.DisputeFlag.String(),
		"debtor_concentration": f.DebtorConcentration.String(),
		"market_volatility":    f.MarketVolatility.String(),
		"debtor_risk":          f.DebtorRisk.String(),
	}
}

func actorOf(r *http.Request) Actor {
	actor := httpserver.ActorFrom(r.Context())
	return Actor{OrganizationID: actor.OrganizationID, Operator: actor.Operator}
}
