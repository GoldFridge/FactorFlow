package hedera

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

/*
Mirror is an independent reader of what a network actually holds.

The settlement saga confirms a transfer twice, and the two confirmations are only worth
having if they come from different places. The first is the receipt handed back by the node
that accepted the transaction — which is that node's word for it. This is the second: a
public mirror, queried over HTTPS with no credentials, which has no idea who submitted what
and answers the same question to anybody who asks.

It needs no key, which is worth saying plainly: verifying a Hedera payment is something the
platform can do for itself, and a third-party verifier would only be another party to trust.
*/
type Mirror struct {
	http    *http.Client
	baseURL string
}

// MirrorTimeout bounds one query. A mirror that is slow is a transfer that stays unconfirmed
// for another round of the saga, which is exactly the outcome the saga is built for.
const MirrorTimeout = 15 * time.Second

// NewMirror returns a reader for a network. An empty base URL selects the public mirror for
// that network.
func NewMirror(network, baseURL string) *Mirror {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		base = MirrorURL(network)
	}
	return &Mirror{http: &http.Client{Timeout: MirrorTimeout}, baseURL: base}
}

// MirrorURL is the public mirror of a network.
func MirrorURL(network string) string {
	switch strings.ToLower(strings.TrimSpace(network)) {
	case Mainnet:
		return "https://mainnet-public.mirrornode.hedera.com"
	case Previewnet:
		return "https://previewnet.mirrornode.hedera.com"
	default:
		return "https://testnet.mirrornode.hedera.com"
	}
}

// MirrorRecord is what the mirror knows about one transaction.
type MirrorRecord struct {
	TransactionID string
	// Found reports whether the mirror has heard of it at all. A transaction it cannot see
	// has not necessarily failed: mirrors lag consensus by seconds, and the difference
	// between "not yet" and "never" is the whole reason this is a separate step.
	Found  bool
	Status string
	// ConsensusAt is when the network agreed it happened.
	ConsensusAt time.Time
	// Transfers are the token movements the transaction actually performed, which is what
	// makes this a check rather than a restatement of what we asked for.
	Transfers []MirrorTransfer
	// Credits are the same for the network's own unit, in tinybars.
	Credits []MirrorCredit
	// Memo is the note the sender attached. A payment for one question must not buy the
	// answer to another, and on a plain transfer the memo is where that binding lives.
	Memo string
}

// MirrorTransfer is one account's change in one token.
type MirrorTransfer struct {
	TokenID string
	Account string
	Amount  int64
}

// MirrorCredit is one account's change in the network's own unit, in tinybars.
type MirrorCredit struct {
	Account string
	Amount  int64
}

/*
Paid reports whether this transaction credited an account with at least an amount of the
network's own unit.

At least, rather than exactly: a payer that rounded up has paid, and refusing their answer
over a surplus they chose to give would be pedantry with somebody else's money. Paying less
than the quoted price is a different matter and fails.
*/
func (r MirrorRecord) Paid(account string, tinybars int64) bool {
	for _, credit := range r.Credits {
		if credit.Account == account && credit.Amount >= tinybars {
			return true
		}
	}
	return false
}

// PaidBy reports whether an account was debited by this transaction, which is how the
// platform checks that the payer is who the proof claims rather than who it names.
func (r MirrorRecord) PaidBy(account string) bool {
	for _, credit := range r.Credits {
		if credit.Account == account && credit.Amount < 0 {
			return true
		}
	}
	return false
}

// Succeeded reports the network's own verdict.
func (r MirrorRecord) Succeeded() bool { return r.Found && r.Status == "SUCCESS" }

// Moved reports whether the transaction credited an account with an amount of a token. It is
// how a caller checks that what landed is what it planned, rather than trusting its own
// record of what it submitted.
func (r MirrorRecord) Moved(tokenID, account string, amount int64) bool {
	for _, transfer := range r.Transfers {
		if transfer.TokenID == tokenID && transfer.Account == account && transfer.Amount == amount {
			return true
		}
	}
	return false
}

type mirrorResponse struct {
	Transactions []struct {
		TransactionID string `json:"transaction_id"`
		Result        string `json:"result"`
		ConsensusAt   string `json:"consensus_timestamp"`
		MemoBase64    string `json:"memo_base64"`
		Transfers     []struct {
			Account string `json:"account"`
			Amount  int64  `json:"amount"`
		} `json:"transfers"`
		TokenTransfers []struct {
			TokenID string `json:"token_id"`
			Account string `json:"account"`
			Amount  int64  `json:"amount"`
		} `json:"token_transfers"`
	} `json:"transactions"`
}

/*
Transaction asks the mirror what became of a transaction.

An unknown transaction is not an error. The mirror is behind consensus by design, and a
caller that treated "not yet indexed" as failure would resubmit a transfer that had already
happened — which is the one mistake this whole design exists to prevent.
*/
func (m *Mirror) Transaction(ctx context.Context, transactionID string) (MirrorRecord, error) {
	id := MirrorTransactionID(transactionID)
	if id == "" {
		return MirrorRecord{}, fmt.Errorf("hedera: no transaction id to look up")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		m.baseURL+"/api/v1/transactions/"+id, http.NoBody)
	if err != nil {
		return MirrorRecord{}, fmt.Errorf("hedera: building the mirror request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	res, err := m.http.Do(req)
	if err != nil {
		return MirrorRecord{}, fmt.Errorf("hedera: asking the mirror: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode == http.StatusNotFound {
		return MirrorRecord{TransactionID: transactionID}, nil
	}
	if res.StatusCode != http.StatusOK {
		return MirrorRecord{}, fmt.Errorf("hedera: the mirror answered %d", res.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return MirrorRecord{}, fmt.Errorf("hedera: reading the mirror's answer: %w", err)
	}

	var parsed mirrorResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return MirrorRecord{}, fmt.Errorf("hedera: the mirror's answer was not JSON: %w", err)
	}
	if len(parsed.Transactions) == 0 {
		return MirrorRecord{TransactionID: transactionID}, nil
	}

	first := parsed.Transactions[0]
	record := MirrorRecord{
		TransactionID: first.TransactionID,
		Found:         true,
		Status:        first.Result,
		ConsensusAt:   consensusTime(first.ConsensusAt),
	}
	for _, transfer := range first.TokenTransfers {
		record.Transfers = append(record.Transfers, MirrorTransfer{
			TokenID: transfer.TokenID,
			Account: transfer.Account,
			Amount:  transfer.Amount,
		})
	}
	for _, credit := range first.Transfers {
		record.Credits = append(record.Credits, MirrorCredit{
			Account: credit.Account,
			Amount:  credit.Amount,
		})
	}
	if memo, err := base64.StdEncoding.DecodeString(first.MemoBase64); err == nil {
		record.Memo = string(memo)
	}
	return record, nil
}

/*
MirrorTransactionID converts an SDK transaction id into the form the mirror indexes.

The SDK writes 0.0.1234@1699999999.123456789; the mirror wants 0.0.1234-1699999999-123456789.
It is a small difference and a costly one to get wrong — the mirror simply answers "not
found", which the saga reads as "not yet", and a transfer that succeeded would wait forever.
*/
func MirrorTransactionID(transactionID string) string {
	id := strings.TrimSpace(transactionID)
	if id == "" {
		return ""
	}
	if !strings.Contains(id, "@") {
		return id
	}

	account, stamp, _ := strings.Cut(id, "@")
	return account + "-" + strings.Replace(stamp, ".", "-", 1)
}

// consensusTime parses the mirror's seconds.nanoseconds stamp, and reports the zero time
// rather than an error: a record that arrived without a readable timestamp is still a record
// that the transaction exists.
func consensusTime(stamp string) time.Time {
	seconds, nanos, found := strings.Cut(strings.TrimSpace(stamp), ".")
	if !found {
		return time.Time{}
	}

	var sec, nsec int64
	if _, err := fmt.Sscanf(seconds, "%d", &sec); err != nil {
		return time.Time{}
	}
	if _, err := fmt.Sscanf(nanos, "%d", &nsec); err != nil {
		return time.Time{}
	}
	return time.Unix(sec, nsec).UTC()
}
