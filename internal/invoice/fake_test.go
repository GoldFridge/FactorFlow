package invoice_test

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// The fakes below let the service and handler tests run without a database. They are not a
// substitute for the repository tests: those run against real PostgreSQL, because that is
// where the SQL, the constraints and the transactions actually live. What these fakes are
// for is the layer above, where the rules under test are authorization, composition and
// which writes happen together.

// fakeQuerier records the statements a service executes directly, which is how a test sees
// that an outbox event was published inside the same unit of work.
type fakeQuerier struct {
	mu    sync.Mutex
	execs []string
}

func (f *fakeQuerier) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.execs = append(f.execs, sql)
	return pgconn.NewCommandTag("INSERT 0 1"), nil
}

func (f *fakeQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeQuerier: Query is not used by the service layer")
}

func (f *fakeQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("fakeQuerier: QueryRow is not used by the service layer")
}

// published reports whether a statement touching the given table was executed.
func (f *fakeQuerier) published(table string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, sql := range f.execs {
		if strings.Contains(sql, table) {
			return true
		}
	}
	return false
}

// fakeDB runs "transactions" that either apply or discard the repository's writes, so a
// test can check that a failed unit of work leaves nothing behind.
type fakeDB struct {
	querier *fakeQuerier
	repo    *memRepository
	// failCommit makes the transaction fail after the body ran, standing in for a commit
	// that never lands.
	failCommit error
}

func newFakeDB(repo *memRepository) *fakeDB {
	return &fakeDB{querier: &fakeQuerier{}, repo: repo}
}

func (f *fakeDB) InTx(ctx context.Context, fn func(postgres.Querier) error) error {
	f.repo.begin()

	if err := fn(f.querier); err != nil {
		f.repo.rollback()
		return err
	}
	if f.failCommit != nil {
		f.repo.rollback()
		return f.failCommit
	}
	f.repo.commit()
	return nil
}

func (f *fakeDB) Querier() postgres.Querier { return f.querier }

// memRepository is an in-memory invoice repository with the same version semantics as the
// PostgreSQL one, including a snapshot it can roll back to.
type memRepository struct {
	mu        sync.Mutex
	invoices  map[uuid.UUID]invoice.Invoice
	documents map[uuid.UUID]invoice.Document

	snapshotInvoices  map[uuid.UUID]invoice.Invoice
	snapshotDocuments map[uuid.UUID]invoice.Document

	// failUpdate, when set, makes the next Update fail, standing in for a lost race.
	failUpdate error
}

func newMemRepository() *memRepository {
	return &memRepository{
		invoices:  map[uuid.UUID]invoice.Invoice{},
		documents: map[uuid.UUID]invoice.Document{},
	}
}

func (m *memRepository) begin() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.snapshotInvoices = maps(m.invoices)
	m.snapshotDocuments = maps(m.documents)
}

func (m *memRepository) rollback() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.snapshotInvoices != nil {
		m.invoices = m.snapshotInvoices
		m.documents = m.snapshotDocuments
	}
	m.snapshotInvoices, m.snapshotDocuments = nil, nil
}

func (m *memRepository) commit() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.snapshotInvoices, m.snapshotDocuments = nil, nil
}

func (m *memRepository) Create(_ context.Context, _ postgres.Querier, inv *invoice.Invoice) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.invoices[inv.ID]; exists {
		return apperr.Conflictf("invoice %s already exists", inv.ID)
	}
	m.invoices[inv.ID] = *inv
	return nil
}

func (m *memRepository) Get(_ context.Context, _ postgres.Querier, id uuid.UUID) (*invoice.Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.invoices[id]
	if !ok {
		return nil, apperr.NotFoundf("invoice %s", id)
	}
	copied := stored
	return &copied, nil
}

func (m *memRepository) Update(_ context.Context, _ postgres.Querier, inv *invoice.Invoice, expectedVersion int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failUpdate != nil {
		return m.failUpdate
	}
	stored, ok := m.invoices[inv.ID]
	if !ok {
		return apperr.NotFoundf("invoice %s", inv.ID)
	}
	if stored.Version != expectedVersion {
		return apperr.Conflictf("invoice %s was modified by another writer", inv.ID)
	}
	m.invoices[inv.ID] = *inv
	return nil
}

func (m *memRepository) ListByIssuer(_ context.Context, _ postgres.Querier, issuerID uuid.UUID, limit int) ([]*invoice.Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []*invoice.Invoice
	for _, stored := range m.invoices {
		if stored.IssuerID != issuerID {
			continue
		}
		copied := stored
		out = append(out, &copied)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memRepository) ListByStatus(_ context.Context, _ postgres.Querier, status invoice.Status, limit int) ([]*invoice.Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []*invoice.Invoice
	for _, stored := range m.invoices {
		if stored.Status != status {
			continue
		}
		copied := stored
		out = append(out, &copied)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memRepository) SaveDocument(_ context.Context, _ postgres.Querier, doc *invoice.Document) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.documents[doc.InvoiceID] = *doc
	return nil
}

func (m *memRepository) GetDocument(_ context.Context, _ postgres.Querier, invoiceID uuid.UUID) (*invoice.Document, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stored, ok := m.documents[invoiceID]
	if !ok {
		return nil, apperr.NotFoundf("document for invoice %s", invoiceID)
	}
	copied := stored
	return &copied, nil
}

func maps[K comparable, V any](in map[K]V) map[K]V {
	out := make(map[K]V, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// compile-time check that the fake satisfies the port the service depends on.
var _ invoice.Repository = (*memRepository)(nil)
