package documents

import (
	"encoding/base64"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/httpserver"
	"github.com/GoldFridge/factorflow/internal/platform/objects"
)

// UploadLimit bounds an upload request. It is the object limit plus the room base64 costs,
// because the ciphertext arrives encoded.
const UploadLimit = objects.MaxObjectBytes*4/3 + 1024

// Handler exposes the encrypted upload.
type Handler struct {
	service *Service
}

// NewHandler returns the handler.
func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// Routes registers the endpoints.
//
// The upload gets a larger body limit than the rest of the API: everything else is a small
// JSON document, and one route needing megabytes is not a reason to let every route have them.
func (h *Handler) Routes(r chi.Router) {
	r.Route("/invoices/{invoiceID}/document/content", func(content chi.Router) {
		content.Use(httpserver.MaxBodyBytes(UploadLimit))
		content.Post("/", h.upload)
		content.Get("/", h.fetch)
	})
}

// uploadRequest carries the ciphertext as base64.
//
// Base64 costs a third in size and buys one thing worth having: the upload is an ordinary
// JSON request, so it goes through the same decoding, the same limits and the same problem
// documents as everything else rather than being a second transport with its own rules.
type uploadRequest struct {
	Ciphertext string `json:"ciphertext"`
	MIME       string `json:"mime"`
	KeyRef     string `json:"key_ref"`
}

type documentResponse struct {
	InvoiceID  string `json:"invoice_id"`
	ObjectKey  string `json:"object_key"`
	CipherHash string `json:"cipher_hash"`
	KeyRef     string `json:"key_ref"`
	MIME       string `json:"mime"`
	SizeBytes  int64  `json:"size_bytes"`
	UploadedAt string `json:"uploaded_at"`
	Status     string `json:"status"`
}

func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	var body uploadRequest
	if err := httpserver.DecodeJSON(r, &body); err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	ciphertext, err := base64.StdEncoding.DecodeString(body.Ciphertext)
	if err != nil {
		httpserver.WriteProblem(w, r, apperr.Invalid("ciphertext", "must be base64"))
		return
	}

	result, err := h.service.Upload(r.Context(), actorOf(r), UploadParams{
		InvoiceID:  id,
		Ciphertext: ciphertext,
		MIME:       body.MIME,
		KeyRef:     body.KeyRef,
	})
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	httpserver.WriteJSON(w, r, http.StatusCreated, documentResponse{
		InvoiceID:  result.Document.InvoiceID.String(),
		ObjectKey:  result.Document.ObjectKey,
		CipherHash: result.Document.CipherHash,
		KeyRef:     result.Document.KeyRef,
		MIME:       result.Document.MIME,
		SizeBytes:  result.Document.SizeBytes,
		UploadedAt: result.Document.UploadedAt.Format(time.RFC3339),
		Status:     result.Invoice.Status.String(),
	})
}

// fetch hands the ciphertext back to its owner, as bytes rather than JSON: it is not text,
// and only the browser holding the key can make anything of it.
func (h *Handler) fetch(w http.ResponseWriter, r *http.Request) {
	id, err := invoiceIDOf(r)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	object, err := h.service.Fetch(r.Context(), actorOf(r), id)
	if err != nil {
		httpserver.WriteProblem(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Cipher-Hash", object.CipherHash)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(object.Ciphertext)
}

func invoiceIDOf(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, "invoiceID"))
	if err != nil {
		return uuid.Nil, apperr.Invalid("invoice_id", "must be a UUID")
	}
	return id, nil
}

func actorOf(r *http.Request) Actor {
	actor := httpserver.ActorFrom(r.Context())
	return Actor{OrganizationID: actor.OrganizationID, Operator: actor.Operator}
}
