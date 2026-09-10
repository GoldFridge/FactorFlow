package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GoldFridge/factorflow/internal/app/agents"
	"github.com/GoldFridge/factorflow/internal/payments"
	"github.com/GoldFridge/factorflow/internal/platform/config"
	"github.com/GoldFridge/factorflow/internal/platform/hedera"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

/*
runAgent is the machine customer: a program that buys one answer.

It exists because the claim being made is not "the platform can charge for an endpoint" but
"a program with a wallet and no account here can buy a financial opinion in one exchange".
Those are different claims, and only the second is demonstrated by something that actually
meets the 402, pays a real network, and comes back with proof.

	factorflow agent risk-quote <invoice-id>
	factorflow agent auction-recommendation <auction-id>

It uses its own Hedera account, not the platform's. A demo where the seller pays itself
proves nothing at all.
*/
func runAgent(args []string) error {
	if len(args) == 0 {
		return errors.New(
			"usage: factorflow agent risk-quote [face] [days] | agent auction-recommendation <auction-id>")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})))

	route, body, err := question(args)
	if err != nil {
		return err
	}

	agent, err := agentWallet(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = agent.client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	endpoint := strings.TrimRight(envOr("FF_AGENT_API", "http://localhost:8080"), "/") + route

	// One: ask, and be refused with a price.
	quote, err := askForAPrice(ctx, endpoint, body)
	if err != nil {
		return err
	}
	slog.Info("the platform asked to be paid",
		slog.String("amount", quote.Amount+" "+quote.Currency),
		slog.String("pay_to", quote.PayTo),
		slog.String("network", quote.Network),
		slog.String("nonce", quote.Nonce),
		slog.String("expires_at", quote.ExpiresAt))

	price, err := money.Parse(quote.Amount, money.Currency(quote.Currency))
	if err != nil {
		return fmt.Errorf("the quoted price %q %q is not an amount this agent can pay: %w",
			quote.Amount, quote.Currency, err)
	}

	// Two: pay it, on the network the quote named, with the nonce in the memo so the
	// payment is bound to this question and no other.
	receipt, err := agent.client.Pay(ctx, quote.PayTo, price.Minor(), quote.Nonce)
	if err != nil {
		return fmt.Errorf("paying: %w", err)
	}
	slog.Info("paid",
		slog.String("transaction", receipt.TransactionID),
		slog.String("status", receipt.Status),
		slog.String("explorer", receipt.ExplorerURL))

	// Three: ask again, with the proof. The platform reads the network itself; nothing here
	// is taken on the agent's word.
	//
	// The first attempt is usually refused, and correctly: a mirror lags consensus by
	// seconds, and a platform that confirmed a payment it could not yet see would be taking
	// the payer's word for it. So the agent waits and asks again, which is what "the money
	// is on its way" looks like from the paying side.
	proof := payments.EncodePayment(payments.Payment{
		Nonce:  quote.Nonce,
		Payer:  agent.accountID,
		TxID:   receipt.TransactionID,
		Amount: price,
	}, quote.Network)

	var (
		answer        string
		receiptHeader string
	)
	for attempt := 1; ; attempt++ {
		answer, receiptHeader, err = askAgain(ctx, endpoint, body, proof)
		if err == nil {
			break
		}
		if !errors.Is(err, errNotYetVisible) || attempt >= paymentAttempts {
			return err
		}

		slog.Info("the platform cannot see the payment yet; waiting",
			slog.Int("attempt", attempt), slog.Duration("in", paymentBackoff))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(paymentBackoff):
		}
	}

	slog.Info("answered", slog.String("payment_receipt", receiptHeader))
	fmt.Println(answer)
	return nil
}

/*
question builds what the agent is buying an answer to.

The risk quote takes the facts of a receivable rather than an identifier, which is the whole
point of selling it: a counterparty can price paper this platform has never seen, without
uploading anything to it. The feature vector is fixed here so the demo asks the same
question twice and gets the same answer — the model is deterministic, and that is the claim.
*/
func question(args []string) (string, []byte, error) {
	switch args[0] {
	case "risk-quote":
		face, days := "15000.00", int64(60)
		if len(args) > 1 {
			face = args[1]
		}
		if len(args) > 2 {
			parsed, err := strconv.ParseInt(args[2], 10, 64)
			if err != nil || parsed <= 0 {
				return "", nil, fmt.Errorf("days to due must be a positive whole number, got %q", args[2])
			}
			days = parsed
		}

		body, err := json.Marshal(map[string]any{
			"face":        face,
			"currency":    "USD",
			"days_to_due": days,
			"features": map[string]string{
				"dso_norm":             "0.40",
				"late_payment_rate":    "0.15",
				"dispute_flag":         "0",
				"debtor_concentration": "0.35",
				"debtor_risk":          "0.20",
			},
			"mitigations": map[string]bool{"recourse": true, "collateralized": false},
		})
		return agents.RouteRiskQuote, body, err

	case "auction-recommendation":
		if len(args) < 2 {
			return "", nil, errors.New("usage: factorflow agent auction-recommendation <auction-id>")
		}
		body, err := json.Marshal(map[string]string{"auction_id": args[1]})
		return agents.RouteRecommendation, body, err

	default:
		return "", nil, fmt.Errorf("unknown paid endpoint %q", args[0])
	}
}

// errNotYetVisible is the platform saying it cannot see the payment yet, which is a reason
// to wait rather than a refusal to argue with.
var errNotYetVisible = errors.New("the payment is not visible to the platform yet")

// How long a machine customer waits for a mirror to catch up before giving up.
const (
	paymentAttempts = 10
	paymentBackoff  = 4 * time.Second
)

// agentWallet is the machine customer's own account and key.
type wallet struct {
	client    *hedera.Client
	accountID string
}

func agentWallet(cfg config.Config) (wallet, error) {
	accountID := strings.TrimSpace(os.Getenv("FF_AGENT_ACCOUNT_ID"))
	key := strings.TrimSpace(os.Getenv("FF_AGENT_PRIVATE_KEY"))
	if accountID == "" || key == "" {
		return wallet{}, errors.New(
			"this agent needs its own account: set FF_AGENT_ACCOUNT_ID and FF_AGENT_PRIVATE_KEY")
	}

	client, err := hedera.Connect(hedera.Config{
		Network:    cfg.Providers.HederaNetwork,
		AccountID:  accountID,
		PrivateKey: key,
	})
	if err != nil {
		return wallet{}, err
	}
	return wallet{client: client, accountID: accountID}, nil
}

// quotedPrice is the one payment option a 402 offered.
type quotedPrice struct {
	Scheme    string
	Network   string
	Asset     string
	PayTo     string
	Amount    string
	Currency  string
	Nonce     string
	ExpiresAt string
}

// askForAPrice makes the request that is meant to be refused.
func askForAPrice(ctx context.Context, endpoint string, body []byte) (quotedPrice, error) {
	res, err := post(ctx, endpoint, body, "")
	if err != nil {
		return quotedPrice{}, err
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return quotedPrice{}, err
	}
	if res.StatusCode != http.StatusPaymentRequired {
		return quotedPrice{}, fmt.Errorf(
			"expected 402 with a price, got %d: %s", res.StatusCode, strings.TrimSpace(string(raw)))
	}

	var quote struct {
		Accepts []struct {
			Scheme    string `json:"scheme"`
			Network   string `json:"network"`
			Asset     string `json:"asset"`
			PayTo     string `json:"pay_to"`
			Amount    string `json:"amount"`
			Currency  string `json:"currency"`
			Nonce     string `json:"nonce"`
			ExpiresAt string `json:"expires_at"`
		} `json:"accepts"`
	}
	if err := json.Unmarshal(raw, &quote); err != nil {
		return quotedPrice{}, fmt.Errorf("the 402 was not a price this agent understands: %w", err)
	}
	if len(quote.Accepts) == 0 {
		return quotedPrice{}, errors.New("the platform refused payment but named no way to pay")
	}

	first := quote.Accepts[0]
	return quotedPrice(first), nil
}

// askAgain presents the proof and reads the answer.
func askAgain(ctx context.Context, endpoint string, body []byte, proof string) (string, string, error) {
	res, err := post(ctx, endpoint, body, proof)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	if res.StatusCode == http.StatusServiceUnavailable {
		return "", "", fmt.Errorf("%w: %s", errNotYetVisible, strings.TrimSpace(string(raw)))
	}
	if res.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("the platform refused the payment with %d: %s",
			res.StatusCode, strings.TrimSpace(string(raw)))
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		return string(raw), res.Header.Get(payments.PaymentResponseHeader), nil
	}
	return pretty.String(), res.Header.Get(payments.PaymentResponseHeader), nil
}

func post(ctx context.Context, endpoint string, body []byte, proof string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if proof != "" {
		req.Header.Set(payments.PaymentHeader, proof)
	}

	res, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", endpoint, err)
	}
	return res, nil
}

// envOr is the agent's own tiny copy: it runs before any configuration is loaded.
func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
