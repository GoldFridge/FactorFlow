package invoice

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/audit"
	"github.com/GoldFridge/factorflow/internal/platform/money"
	"github.com/GoldFridge/factorflow/internal/platform/outbox"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// EntityType names invoices in the audit timeline.
const EntityType = "invoice"

// Audit actions this module records.
const (
	ActionCreated               = "invoice.created"
	ActionDocumentAttached      = "invoice.document_attached"
	ActionAssessmentRequested   = "invoice.assessment_requested"
	ActionTokenizationRequested = "invoice.tokenization_requested"
	ActionApproved              = "invoice.approved"
	ActionRejected              = "invoice.rejected"
	ActionAuctionOpened         = "invoice.auction_opened"
)

// Outbox topics this module publishes.
const (
	// TopicAssess asks the confidential workflow to assess an invoice.
	TopicAssess = "invoice.assess"
	// TopicTokenize asks the asset issuer to mint the approved receivable.
	TopicTokenize = "invoice.tokenize"
)

// TxRunner is the transaction boundary the service needs.
//
// The service depends on this rather than on *postgres.DB so its rules can be tested
// without a database, while production passes the real pool and gets real transactions.
type TxRunner interface {
	InTx(ctx context.Context, fn func(postgres.Querier) error) error
	Querier() postgres.Querier
}

// Actor is the authenticated caller, as far as this module is concerned.
//
// The service never trusts an organization id from a request body: a client that could
// name its own organization could act as any organization.
type Actor struct {
	OrganizationID uuid.UUID
	Operator       bool
}

// Service is the application layer of the invoice module: it enforces who may do what,
// composes repository writes with their outbox events, and leaves every state rule to the
// aggregate.
type Service struct {
	db    TxRunner
	repo  Repository
	audit audit.Recorder
	now   func() time.Time
	ids   func() uuid.UUID
}

// NewService wires the service. The clock and the id source are injected so a test can
// produce a byte-identical result twice.
func NewService(db TxRunner, repo Repository, recorder audit.Recorder, now func() time.Time, ids func() uuid.UUID) *Service {
	if now == nil {
		now = time.Now
	}
	if ids == nil {
		ids = uuid.New
	}
	if recorder == nil {
		recorder = audit.Discard{}
	}
	return &Service{db: db, repo: repo, audit: recorder, now: now, ids: ids}
}

// auditState is what an audit entry's before and after hashes are taken over: the facts a
// reader checking the timeline against the record can compare, and nothing about the
// document behind the invoice.
func auditState(inv *Invoice) map[string]any {
	return map[string]any{"status": inv.Status.String(), "version": inv.Version}
}

// actorRef names the caller in the timeline. Work with no organization behind it is the
// platform acting on its own, which is what SystemActor means.
func actorRef(actor Actor) string {
	if actor.OrganizationID == uuid.Nil {
		return audit.SystemActor
	}
	return actor.OrganizationID.String()
}

// record appends one timeline entry inside the caller transaction, so the entry and the
// change it describes commit together or neither does.
func (s *Service) record(ctx context.Context, q postgres.Querier, actor Actor, action string, inv *Invoice, before map[string]any) error {
	return s.audit.Record(ctx, q,
		audit.Of(ctx, actorRef(actor), action, EntityType, inv.ID.String(), s.now()).
			Between(before, auditState(inv)).
			With("status", inv.Status.String()))
}

// CreateParams are the facts an issuer supplies for a new draft.
type CreateParams struct {
	DebtorRef string
	Number    string
	Face      money.Amount
	IssuedAt  time.Time
	DueAt     time.Time
}

// Create records a draft invoice for the caller's organization.
func (s *Service) Create(ctx context.Context, actor Actor, p CreateParams) (*Invoice, error) {
	if actor.OrganizationID == uuid.Nil {
		return nil, apperr.Forbiddenf("an organization is required to create an invoice")
	}

	inv, err := New(NewParams{
		ID:        s.ids(),
		IssuerID:  actor.OrganizationID,
		DebtorRef: p.DebtorRef,
		Number:    p.Number,
		Face:      p.Face,
		IssuedAt:  p.IssuedAt,
		DueAt:     p.DueAt,
	}, s.now())
	if err != nil {
		return nil, err
	}

	if err := s.db.InTx(ctx, func(q postgres.Querier) error {
		if err := s.repo.Create(ctx, q, inv); err != nil {
			// The storage rule is one live receivable per set of terms, across the venue.
			// What it catches is the same paper offered twice, and the refusal has to say
			// so: "conflict" alone reads like a retry, and a seller who is not defrauding
			// anybody needs to know which of their facts collided.
			if apperr.IsConflict(err) {
				return apperr.Conflictf(
					"a receivable from %s numbered %s for %s due %s is already being financed",
					inv.DebtorRef, inv.Number, inv.Face, inv.DueAt.UTC().Format("2006-01-02"))
			}
			return err
		}
		return s.record(ctx, q, actor, ActionCreated, inv, nil)
	}); err != nil {
		return nil, err
	}
	return inv, nil
}

// AttachDocument records an encrypted upload and moves the invoice out of DRAFT.
//
// Both writes happen in one transaction: an invoice marked UPLOADED with no document, or a
// document with no state change, would each break the assessment step that follows.
func (s *Service) AttachDocument(ctx context.Context, actor Actor, invoiceID uuid.UUID, p NewDocumentParams) (*Invoice, error) {
	p.InvoiceID = invoiceID

	doc, err := NewDocument(p, s.now())
	if err != nil {
		return nil, err
	}

	var updated *Invoice
	err = s.db.InTx(ctx, func(q postgres.Querier) error {
		inv, err := s.load(ctx, q, actor, invoiceID)
		if err != nil {
			return err
		}

		before := auditState(inv)
		expectedVersion := inv.Version
		if err := inv.MarkUploaded(s.now()); err != nil {
			return err
		}
		if err := s.repo.SaveDocument(ctx, q, doc); err != nil {
			return err
		}
		if err := s.repo.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}
		// The digest is recorded, never the document: the timeline says which ciphertext
		// was attached without becoming a second copy of it.
		if err := s.audit.Record(ctx, q,
			audit.Of(ctx, actorRef(actor), ActionDocumentAttached, EntityType, inv.ID.String(), s.now()).
				Between(before, auditState(inv)).
				With("status", inv.Status.String()).
				With("cipher_hash", doc.CipherHash)); err != nil {
			return err
		}

		updated = inv
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// assessCommand is the payload the confidential workflow consumes.
//
// It names the invoice and the ciphertext to fetch, never the document itself and never the
// data key: the key travels to the TEE as a confidential input, not through the queue.
type assessCommand struct {
	InvoiceID  uuid.UUID `json:"invoice_id"`
	ObjectKey  string    `json:"object_key"`
	CipherHash string    `json:"cipher_hash"`
	KeyRef     string    `json:"key_ref"`
	MIME       string    `json:"mime"`
	Attempt    int       `json:"attempt"`
}

// RequestAssessment queues the confidential assessment of an uploaded invoice.
//
// The state change and the queued command are committed together, so an invoice can never
// sit in EXTRACTING with nothing on its way to assess it.
func (s *Service) RequestAssessment(ctx context.Context, actor Actor, invoiceID uuid.UUID, traceID string) (*Invoice, error) {
	var updated *Invoice

	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		inv, err := s.load(ctx, q, actor, invoiceID)
		if err != nil {
			return err
		}

		doc, err := s.repo.GetDocument(ctx, q, invoiceID)
		if err != nil {
			return err
		}

		before := auditState(inv)
		expectedVersion := inv.Version
		if err := inv.StartAssessment(s.now()); err != nil {
			return err
		}
		if err := s.repo.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}
		if err := s.record(ctx, q, actor, ActionAssessmentRequested, inv, before); err != nil {
			return err
		}

		if err := outbox.Publish(ctx, q, TopicAssess, assessCommand{
			InvoiceID:  inv.ID,
			ObjectKey:  doc.ObjectKey,
			CipherHash: doc.CipherHash,
			KeyRef:     doc.KeyRef,
			MIME:       doc.MIME,
		}, traceID, s.now()); err != nil {
			return err
		}

		updated = inv
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// tokenizeCommand is the payload the issuance worker consumes. It names the invoice and
// nothing else: the worker reads the facts it needs from storage, so a command that sat in
// the queue cannot carry a stale face value onto the chain.
type tokenizeCommand struct {
	InvoiceID uuid.UUID `json:"invoice_id"`
	IssuerID  uuid.UUID `json:"issuer_id"`
}

// RequestTokenization queues issuance of an approved invoice.
//
// The state change and the queued command commit together, so an invoice can never sit in
// TOKENIZING with nothing on its way to mint it.
func (s *Service) RequestTokenization(ctx context.Context, actor Actor, invoiceID uuid.UUID, traceID string) (*Invoice, error) {
	var updated *Invoice

	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		inv, err := s.load(ctx, q, actor, invoiceID)
		if err != nil {
			return err
		}

		before := auditState(inv)
		expectedVersion := inv.Version
		if err := inv.StartTokenization(s.now()); err != nil {
			return err
		}
		if err := s.repo.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}
		if err := s.record(ctx, q, actor, ActionTokenizationRequested, inv, before); err != nil {
			return err
		}

		if err := outbox.Publish(ctx, q, TopicTokenize, tokenizeCommand{
			InvoiceID: inv.ID,
			IssuerID:  inv.IssuerID,
		}, traceID, s.now()); err != nil {
			return err
		}

		updated = inv
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// OpenAuction moves a tokenized invoice into an open auction. It is called by the
// application layer once the batch itself exists.
func (s *Service) OpenAuction(ctx context.Context, q postgres.Querier, inv *Invoice) error {
	before := auditState(inv)
	expectedVersion := inv.Version
	if err := inv.OpenAuction(s.now()); err != nil {
		return err
	}
	if err := s.repo.Update(ctx, q, inv, expectedVersion); err != nil {
		return err
	}
	return s.record(ctx, q, Actor{OrganizationID: inv.IssuerID}, ActionAuctionOpened, inv, before)
}

// LoadForAuction returns an invoice the caller may offer, inside the caller's transaction.
func (s *Service) LoadForAuction(ctx context.Context, q postgres.Querier, actor Actor, invoiceID uuid.UUID) (*Invoice, error) {
	return s.load(ctx, q, actor, invoiceID)
}

// Approve records the issuer confirming the extracted facts.
func (s *Service) Approve(ctx context.Context, actor Actor, invoiceID uuid.UUID) (*Invoice, error) {
	return s.mutate(ctx, actor, invoiceID, ActionApproved, func(inv *Invoice) error {
		return inv.Approve(s.now())
	})
}

// Reject ends the lifecycle before tokenization.
func (s *Service) Reject(ctx context.Context, actor Actor, invoiceID uuid.UUID, reason string) (*Invoice, error) {
	return s.mutate(ctx, actor, invoiceID, ActionRejected, func(inv *Invoice) error {
		return inv.Reject(reason, s.now())
	})
}

// Get returns one invoice the caller may see.
func (s *Service) Get(ctx context.Context, actor Actor, invoiceID uuid.UUID) (*Invoice, error) {
	inv, err := s.repo.Get(ctx, s.db.Querier(), invoiceID)
	if err != nil {
		return nil, err
	}
	if err := authorize(actor, inv); err != nil {
		return nil, err
	}
	return inv, nil
}

// GetDocument returns the document metadata of an invoice the caller may see. It returns
// metadata only: the ciphertext lives in object storage and the plaintext nowhere.
func (s *Service) GetDocument(ctx context.Context, actor Actor, invoiceID uuid.UUID) (*Document, error) {
	if _, err := s.Get(ctx, actor, invoiceID); err != nil {
		return nil, err
	}
	return s.repo.GetDocument(ctx, s.db.Querier(), invoiceID)
}

// List returns the caller's own invoices.
func (s *Service) List(ctx context.Context, actor Actor, limit int) ([]*Invoice, error) {
	if actor.OrganizationID == uuid.Nil {
		return nil, apperr.Forbiddenf("an organization is required to list invoices")
	}
	return s.repo.ListByIssuer(ctx, s.db.Querier(), actor.OrganizationID, limit)
}

// mutate loads, applies a command, and stores the result under the version it read.
func (s *Service) mutate(ctx context.Context, actor Actor, invoiceID uuid.UUID, action string, apply func(*Invoice) error) (*Invoice, error) {
	var updated *Invoice

	err := s.db.InTx(ctx, func(q postgres.Querier) error {
		inv, err := s.load(ctx, q, actor, invoiceID)
		if err != nil {
			return err
		}

		before := auditState(inv)
		expectedVersion := inv.Version
		if err := apply(inv); err != nil {
			return err
		}
		if err := s.repo.Update(ctx, q, inv, expectedVersion); err != nil {
			return err
		}
		if err := s.record(ctx, q, actor, action, inv, before); err != nil {
			return err
		}

		updated = inv
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

func (s *Service) load(ctx context.Context, q postgres.Querier, actor Actor, invoiceID uuid.UUID) (*Invoice, error) {
	inv, err := s.repo.Get(ctx, q, invoiceID)
	if err != nil {
		return nil, err
	}
	if err := authorize(actor, inv); err != nil {
		return nil, err
	}
	return inv, nil
}

// authorize allows an invoice's own issuer, and a platform operator.
//
// It reports "not found" rather than "forbidden" for someone else's invoice: telling a
// stranger that an invoice exists is itself a disclosure.
func authorize(actor Actor, inv *Invoice) error {
	if actor.Operator || actor.OrganizationID == inv.IssuerID {
		return nil
	}
	return apperr.NotFoundf("invoice %s", inv.ID)
}
