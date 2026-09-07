package auction

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/risk"
)

// Handler exposes the auction module over HTTP.
//
// Creating an auction is not here: assembling lots needs invoices and their assessments,
// so that endpoint lives in the application layer. What this handler owns is everything
// that happens once a batch exists.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Routes registers the module's endpoints.
func (h *Handler) Routes(r chi.Router) {
	r.Get("/auctions", h.list)
	r.Get("/auctions/{auctionID}", h.get)
	r.Post("/auctions/{auctionID}/open", h.open)
	r.Post("/auctions/{auctionID}/cancel", h.cancel)
	r.Post("/auctions/{auctionID}/clear", h.clear)
	r.Get("/auctions/{auctionID}/bids", h.bids)
	r.Post("/auctions/{auctionID}/bids", h.placeBid)
	r.Get("/auctions/{auctionID}/allocations", h.allocations)
	r.Post("/bids/{bidID}/cancel", h.cancelBid)
}

// bidRequest is the body of POST /auctions/{id}/bids. It is the investor's whole risk
// appetite: what they will spend, what they demand for it, and how much of any one name
// they will hold.
type bidRequest struct {
	Budget         string            `json:"budget"`
	Currency       string            `json:"currency"`
	MinYield       string            `json:"min_yield"`
	MaxGrade       string            `json:"max_grade"`
	MaxTenorDays   int64             `json:"max_tenor_days"`
	MinimumLot     string            `json:"minimum_lot"`
	MaxIssuerShare string            `json:"max_issuer_share"`
	MaxDebtorShare string            `json:"max_debtor_share"`
	MaxGradeShare  map[string]string `json:"max_grade_share"`
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

type lotResponse struct {
	ID           string `json:"id"`
	InvoiceID    string `json:"invoice_id"`
	AssetID      string `json:"asset_id"`
	IssuerID     string `json:"issuer_id"`
	DebtorRef    string `json:"debtor_ref"`
	Supply       string `json:"supply"`
	ReservePrice string `json:"reserve_price"`
	Currency     string `json:"currency"`
	Grade        string `json:"grade"`
	TenorDays    int64  `json:"tenor_days"`
	// ImpliedYield is what an investor earns buying the lot at its reserve price. It is
	// derived, not stored: showing it beside the price is the only way the number an
	// investor actually decides on is visible.
	ImpliedYield string `json:"implied_yield"`
}

type auctionResponse struct {
	ID              string        `json:"id"`
	IssuerID        string        `json:"issuer_id"`
	Status          string        `json:"status"`
	OpensAt         string        `json:"opens_at"`
	ClosesAt        string        `json:"closes_at"`
	Lots            []lotResponse `json:"lots"`
	TotalSupply     string        `json:"total_supply"`
	Currency        string        `json:"currency"`
	SolverVersion   string        `json:"solver_version,omitempty"`
	CertificateHash string        `json:"certificate_hash,omitempty"`
	Reason          string        `json:"reason,omitempty"`
	Version         int64         `json:"version"`
	CreatedAt       string        `json:"created_at"`
	UpdatedAt       string        `json:"updated_at"`
}

type bidResponse struct {
	ID             string            `json:"id"`
	AuctionID      string            `json:"auction_id"`
	InvestorID     string            `json:"investor_id"`
	Budget         string            `json:"budget"`
	Currency       string            `json:"currency"`
	MinYield       string            `json:"min_yield"`
	MaxGrade       string            `json:"max_grade"`
	MaxTenorDays   int64             `json:"max_tenor_days"`
	MinimumLot     string            `json:"minimum_lot"`
	MaxIssuerShare string            `json:"max_issuer_share"`
	MaxDebtorShare string            `json:"max_debtor_share"`
	MaxGradeShare  map[string]string `json:"max_grade_share,omitempty"`
	Status         string            `json:"status"`
	Version        int64             `json:"version"`
	CreatedAt      string            `json:"created_at"`
}

type allocationResponse struct {
	Rank       int    `json:"rank"`
	LotID      string `json:"lot_id"`
	InvoiceID  string `json:"invoice_id"`
	AssetID    string `json:"asset_id"`
	BidID      string `json:"bid_id"`
	InvestorID string `json:"investor_id"`
	Notional   string `json:"notional"`
	Price      string `json:"price"`
	Currency   string `json:"currency"`
}

// rejectionResponse tells a bid which of its own limits refused the batch.
type rejectionResponse struct {
	BidID      string `json:"bid_id"`
	LotID      string `json:"lot_id,omitempty"`
	Constraint string `json:"constraint"`
}

type solutionResponse struct {
	AuctionID       string               `json:"auction_id"`
	SolverVersion   string               `json:"solver_version"`
	CertificateHash string               `json:"certificate_hash"`
	Verified        bool                 `json:"verified"`
	Objective       int64                `json:"objective"`
	TotalNotional   string               `json:"total_notional"`
	TotalCash       string               `json:"total_cash"`
	Currency        string               `json:"currency"`
	Allocations     []allocationResponse `json:"allocations"`
	Rejections      []rejectionResponse  `json:"rejections"`
	BranchNodes     int                  `json:"branch_nodes"`
	RepairRounds    int                  `json:"repair_rounds"`
	LimitReached    bool                 `json:"limit_reached"`
}

type auctionListResponse struct {
	Items []auctionResponse `json:"items"`
}

type bidListResponse struct {
	Items []bidResponse `json:"items"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			httpserver.WriteProblem(w, r, apperr.Invalid("limit", "must be a positive integer"))
			return
		}
		limit = parsed
	}

	auctions, err := h.service.List(r.Context(), actorOf(r), Status(r.URL.Query().Get("status")), limit)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	items := make([]auctionResponse, 0, len(auctions))
	for _, a := range auctions {
		response, err := toAuctionResponse(a)
		if err != nil {
			httpserver.WriteProblem(w, r, err)
			return
		}
		items = append(items, response)
	}
	httpserver.WriteJSON(w, r, http.StatusOK, auctionListResponse{Items: items})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := idOf(r, "auctionID")
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	a, err := h.service.Get(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	h.writeAuction(w, r, http.StatusOK, a)
}

func (h *Handler) open(w http.ResponseWriter, r *http.Request) {
	id, err := idOf(r, "auctionID")
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	a, err := h.service.Open(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	h.writeAuction(w, r, http.StatusOK, a)
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	id, err := idOf(r, "auctionID")
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	var body reasonRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	a, err := h.service.Cancel(r.Context(), actorOf(r), id, body.Reason)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	h.writeAuction(w, r, http.StatusOK, a)
}

func (h *Handler) clear(w http.ResponseWriter, r *http.Request) {
	id, err := idOf(r, "auctionID")
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	solution, err := h.service.Clear(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toSolutionResponse(solution))
}

func (h *Handler) placeBid(w http.ResponseWriter, r *http.Request) {
	auctionID, err := idOf(r, "auctionID")
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	var body bidRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	params, err := body.toParams()
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	bid, err := h.service.PlaceBid(r.Context(), actorOf(r), auctionID, params)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusCreated, toBidResponse(bid))
}

func (h *Handler) cancelBid(w http.ResponseWriter, r *http.Request) {
	bidID, err := idOf(r, "bidID")
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	bid, err := h.service.CancelBid(r.Context(), actorOf(r), bidID)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toBidResponse(bid))
}

func (h *Handler) bids(w http.ResponseWriter, r *http.Request) {
	auctionID, err := idOf(r, "auctionID")
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	bids, err := h.service.Bids(r.Context(), actorOf(r), auctionID)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	items := make([]bidResponse, 0, len(bids))
	for _, bid := range bids {
		items = append(items, toBidResponse(bid))
	}
	httpserver.WriteJSON(w, r, http.StatusOK, bidListResponse{Items: items})
}

func (h *Handler) allocations(w http.ResponseWriter, r *http.Request) {
	auctionID, err := idOf(r, "auctionID")
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	solution, err := h.service.Solution(r.Context(), actorOf(r), auctionID)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, http.StatusOK, toSolutionResponse(solution))
}

func (h *Handler) writeAuction(w http.ResponseWriter, r *http.Request, status int, a *Auction) {
	response, err := toAuctionResponse(a)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}
	httpserver.WriteJSON(w, r, status, response)
}

// toParams converts a bid body into domain types, reporting every malformed field at once.
func (b bidRequest) toParams() (BidParams, error) {
	var violations []error

	currency, err := money.ParseCurrency(b.Currency)
	if err != nil {
		violations = append(violations, apperr.Invalid("currency", "must be a supported ISO 4217 code"))
	}

	params := BidParams{MaxTenorDays: b.MaxTenorDays}

	if currency.IsValid() {
		if params.Budget, err = money.Parse(b.Budget, currency); err != nil {
			violations = append(violations, apperr.Invalid("budget", "must be a decimal amount in %s", currency))
		}
		// An omitted minimum lot means the investor will take any size.
		params.MinimumLot = money.Zero(currency)
		if b.MinimumLot != "" {
			if params.MinimumLot, err = money.Parse(b.MinimumLot, currency); err != nil {
				violations = append(violations, apperr.Invalid("minimum_lot", "must be a decimal amount in %s", currency))
			}
		}
	}

	if params.MinYield, err = money.ParseRate(b.MinYield); err != nil {
		violations = append(violations, apperr.Invalid("min_yield", "must be a decimal fraction such as 0.08"))
	}
	if params.MaxGrade, err = risk.ParseGrade(b.MaxGrade); err != nil {
		violations = append(violations, apperr.Invalid("max_grade", "must be one of A, B, C, D, E"))
	}

	// An omitted exposure share means uncapped, which the domain reads as a zero rate.
	if params.MaxIssuerShare, err = parseOptionalRate("max_issuer_share", b.MaxIssuerShare); err != nil {
		violations = append(violations, err)
	}
	if params.MaxDebtorShare, err = parseOptionalRate("max_debtor_share", b.MaxDebtorShare); err != nil {
		violations = append(violations, err)
	}

	if len(b.MaxGradeShare) > 0 {
		params.MaxGradeShare = make(map[risk.Grade]money.Rate, len(b.MaxGradeShare))
		for rawGrade, rawShare := range b.MaxGradeShare {
			grade, err := risk.ParseGrade(rawGrade)
			if err != nil {
				violations = append(violations, apperr.Invalid("max_grade_share", "unknown grade %q", rawGrade))
				continue
			}
			share, err := money.ParseRate(rawShare)
			if err != nil {
				violations = append(violations, apperr.Invalid("max_grade_share."+rawGrade, "must be a decimal fraction"))
				continue
			}
			params.MaxGradeShare[grade] = share
		}
	}

	if err := errors.Join(violations...); err != nil {
		return BidParams{}, err
	}
	return params, nil
}

func parseOptionalRate(field, raw string) (money.Rate, error) {
	if raw == "" {
		return money.ZeroRate(), nil
	}
	rate, err := money.ParseRate(raw)
	if err != nil {
		return money.Rate{}, apperr.Invalid(field, "must be a decimal fraction such as 0.25")
	}
	return rate, nil
}

func toAuctionResponse(a *Auction) (auctionResponse, error) {
	total, err := a.TotalSupply()
	if err != nil {
		return auctionResponse{}, err
	}

	lots := make([]lotResponse, 0, len(a.Lots))
	for _, lot := range a.Lots {
		yield, err := lot.ImpliedYield()
		if err != nil {
			return auctionResponse{}, err
		}
		lots = append(lots, lotResponse{
			ID:           lot.ID.String(),
			InvoiceID:    lot.InvoiceID.String(),
			AssetID:      lot.AssetID.String(),
			IssuerID:     lot.IssuerID.String(),
			DebtorRef:    lot.DebtorRef,
			Supply:       lot.Supply.String(),
			ReservePrice: lot.ReservePrice.String(),
			Currency:     lot.Supply.Currency().String(),
			Grade:        lot.Grade.String(),
			TenorDays:    lot.TenorDays,
			ImpliedYield: yield.StringFixed(rateScale),
		})
	}

	return auctionResponse{
		ID:              a.ID.String(),
		IssuerID:        a.IssuerID.String(),
		Status:          a.Status.String(),
		OpensAt:         a.OpensAt.Format(time.RFC3339),
		ClosesAt:        a.ClosesAt.Format(time.RFC3339),
		Lots:            lots,
		TotalSupply:     total.String(),
		Currency:        a.Currency().String(),
		SolverVersion:   a.SolverVersion,
		CertificateHash: a.CertificateHash,
		Reason:          a.Reason,
		Version:         a.Version,
		CreatedAt:       a.CreatedAt.Format(time.RFC3339),
		UpdatedAt:       a.UpdatedAt.Format(time.RFC3339),
	}, nil
}

func toBidResponse(b *Bid) bidResponse {
	shares := make(map[string]string, len(b.MaxGradeShare))
	for grade, share := range b.MaxGradeShare {
		shares[grade.String()] = share.StringFixed(rateScale)
	}

	return bidResponse{
		ID:             b.ID.String(),
		AuctionID:      b.AuctionID.String(),
		InvestorID:     b.InvestorID.String(),
		Budget:         b.Budget.String(),
		Currency:       b.Budget.Currency().String(),
		MinYield:       b.MinYield.StringFixed(rateScale),
		MaxGrade:       b.MaxGrade.String(),
		MaxTenorDays:   b.MaxTenorDays,
		MinimumLot:     b.MinimumLot.String(),
		MaxIssuerShare: b.MaxIssuerShare.StringFixed(rateScale),
		MaxDebtorShare: b.MaxDebtorShare.StringFixed(rateScale),
		MaxGradeShare:  shares,
		Status:         b.Status.String(),
		Version:        b.Version,
		CreatedAt:      b.CreatedAt.Format(time.RFC3339),
	}
}

func toSolutionResponse(s *Solution) solutionResponse {
	allocations := make([]allocationResponse, 0, len(s.Allocations))
	for _, allocation := range s.Allocations {
		allocations = append(allocations, allocationResponse{
			Rank:       allocation.Rank,
			LotID:      allocation.LotID.String(),
			InvoiceID:  allocation.InvoiceID.String(),
			AssetID:    allocation.AssetID.String(),
			BidID:      allocation.BidID.String(),
			InvestorID: allocation.InvestorID.String(),
			Notional:   allocation.Notional.String(),
			Price:      allocation.Price.String(),
			Currency:   allocation.Notional.Currency().String(),
		})
	}

	rejections := make([]rejectionResponse, 0, len(s.Rejections))
	for _, rejection := range s.Rejections {
		item := rejectionResponse{
			BidID:      rejection.BidID.String(),
			Constraint: rejection.Constraint.String(),
		}
		if rejection.LotID != uuid.Nil {
			item.LotID = rejection.LotID.String()
		}
		rejections = append(rejections, item)
	}

	return solutionResponse{
		AuctionID:       s.AuctionID.String(),
		SolverVersion:   s.SolverVersion,
		CertificateHash: s.CertificateHash,
		Verified:        s.Verified,
		Objective:       s.Objective,
		TotalNotional:   s.TotalNotional.String(),
		TotalCash:       s.TotalCash.String(),
		Currency:        s.TotalCash.Currency().String(),
		Allocations:     allocations,
		Rejections:      rejections,
		BranchNodes:     s.BranchNodes,
		RepairRounds:    s.RepairRounds,
		LimitReached:    s.LimitReached,
	}
}

func idOf(r *http.Request, param string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		return uuid.Nil, apperr.Invalid(param, "must be a UUID")
	}
	return id, nil
}

// actorOf maps the transport's caller onto this module's actor.
func actorOf(r *http.Request) Actor {
	actor := httpserver.ActorFrom(r.Context())
	return Actor{
		OrganizationID: actor.OrganizationID,
		Eligible:       actor.Eligible,
		Operator:       actor.Operator,
	}
}
