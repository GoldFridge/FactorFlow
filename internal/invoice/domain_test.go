package invoice_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/invoice"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

var (
	testNow      = time.Date(2026, time.September, 5, 12, 0, 0, 0, time.UTC)
	testIssuedAt = testNow
	testDueAt    = testNow.Add(60 * 24 * time.Hour)
)

func validParams() invoice.NewParams {
	return invoice.NewParams{
		ID:        uuid.New(),
		IssuerID:  uuid.New(),
		DebtorRef: "ACME Logistics GmbH",
		Number:    "INV-2026-0042",
		Face:      money.MustParse("10000.00", money.USD),
		IssuedAt:  testIssuedAt,
		DueAt:     testDueAt,
	}
}

func newDraft(t *testing.T) *invoice.Invoice {
	t.Helper()
	inv, err := invoice.New(validParams(), testNow)
	require.NoError(t, err)
	return inv
}

func TestNewCreatesDraft(t *testing.T) {
	t.Parallel()

	p := validParams()
	inv, err := invoice.New(p, testNow)
	require.NoError(t, err)

	assert.Equal(t, invoice.StatusDraft, inv.Status)
	assert.Equal(t, int64(1), inv.Version)
	assert.Equal(t, p.ID, inv.ID)
	assert.Equal(t, money.USD, inv.Currency())
	assert.Equal(t, int64(60), inv.TenorDays())
	assert.Equal(t, testNow, inv.CreatedAt)
	assert.Equal(t, testNow, inv.UpdatedAt)
	assert.Equal(t, time.UTC, inv.DueAt.Location(), "timestamps are normalized to UTC")
}

func TestNewTrimsTextFields(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.DebtorRef = "  ACME Logistics GmbH  "
	p.Number = "\tINV-2026-0042\n"

	inv, err := invoice.New(p, testNow)
	require.NoError(t, err)
	assert.Equal(t, "ACME Logistics GmbH", inv.DebtorRef)
	assert.Equal(t, "INV-2026-0042", inv.Number)
}

func TestNewRejectsBrokenInvariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*invoice.NewParams)
		wantField string
	}{
		{name: "nil id", mutate: func(p *invoice.NewParams) { p.ID = uuid.Nil }, wantField: "id"},
		{name: "nil issuer", mutate: func(p *invoice.NewParams) { p.IssuerID = uuid.Nil }, wantField: "issuer_id"},
		{name: "empty debtor", mutate: func(p *invoice.NewParams) { p.DebtorRef = "   " }, wantField: "debtor_ref"},
		{name: "long debtor", mutate: func(p *invoice.NewParams) { p.DebtorRef = longString(invoice.MaxDebtorRefLen + 1) }, wantField: "debtor_ref"},
		{name: "empty number", mutate: func(p *invoice.NewParams) { p.Number = "" }, wantField: "number"},
		{name: "long number", mutate: func(p *invoice.NewParams) { p.Number = longString(invoice.MaxNumberLen + 1) }, wantField: "number"},
		{name: "zero face", mutate: func(p *invoice.NewParams) { p.Face = money.Zero(money.USD) }, wantField: "face"},
		{name: "negative face", mutate: func(p *invoice.NewParams) { p.Face = money.MustParse("-1.00", money.USD) }, wantField: "face"},
		{name: "face without currency", mutate: func(p *invoice.NewParams) { p.Face = money.Amount{} }, wantField: "face"},
		{name: "missing issue date", mutate: func(p *invoice.NewParams) { p.IssuedAt = time.Time{} }, wantField: "issued_at"},
		{name: "missing due date", mutate: func(p *invoice.NewParams) { p.DueAt = time.Time{} }, wantField: "due_at"},
		{name: "due before issue", mutate: func(p *invoice.NewParams) { p.DueAt = p.IssuedAt.Add(-time.Hour) }, wantField: "due_at"},
		{name: "due equals issue", mutate: func(p *invoice.NewParams) { p.DueAt = p.IssuedAt }, wantField: "due_at"},
		{name: "tenor under a day", mutate: func(p *invoice.NewParams) { p.DueAt = p.IssuedAt.Add(23 * time.Hour) }, wantField: "due_at"},
		{name: "tenor over a year", mutate: func(p *invoice.NewParams) { p.DueAt = p.IssuedAt.Add(400 * 24 * time.Hour) }, wantField: "due_at"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := validParams()
			tc.mutate(&p)

			inv, err := invoice.New(p, testNow)
			require.Nil(t, inv)
			require.ErrorIs(t, err, apperr.ErrValidation)

			fields := apperr.Fields(err)
			require.NotEmpty(t, fields)
			assert.Contains(t, fieldNames(fields), tc.wantField)
		})
	}
}

func TestNewReportsEveryViolationAtOnce(t *testing.T) {
	t.Parallel()

	p := validParams()
	p.DebtorRef = ""
	p.Number = ""
	p.Face = money.Zero(money.USD)

	_, err := invoice.New(p, testNow)
	require.ErrorIs(t, err, apperr.ErrValidation)
	assert.ElementsMatch(t, []string{"debtor_ref", "number", "face"}, fieldNames(apperr.Fields(err)))
}

func TestTenorHelpers(t *testing.T) {
	t.Parallel()

	inv := newDraft(t)

	assert.Equal(t, int64(60), inv.TenorDays())
	assert.Equal(t, int64(60), inv.DaysToDue(testNow))
	assert.Equal(t, int64(30), inv.DaysToDue(testNow.Add(30*24*time.Hour)))
	assert.Equal(t, int64(-5), inv.DaysToDue(inv.DueAt.Add(5*24*time.Hour)))

	assert.False(t, inv.IsOverdue(testNow))
	assert.False(t, inv.IsOverdue(inv.DueAt))
	assert.True(t, inv.IsOverdue(inv.DueAt.Add(time.Second)))
}

func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func fieldNames(fields []*apperr.FieldError) []string {
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.Field)
	}
	return names
}
