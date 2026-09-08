package marketdata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// DefaultGatewayURL is where The Graph's decentralized network answers queries.
const DefaultGatewayURL = "https://gateway.thegraph.com/api/subgraphs/id"

// GraphProvider reads lending markets from subgraphs on The Graph.
//
// It queries several subgraphs with one query text, which is possible because they publish
// the same standardized lending schema. Every market it returns carries the subgraph it came
// from and the block it was indexed at, so a benchmark can be traced back to the exact rows
// behind it rather than being asserted.
type GraphProvider struct {
	endpoint  string
	apiKey    string
	subgraphs []Subgraph
	client    *http.Client
}

// Subgraph is one source of markets.
type Subgraph struct {
	// ID is the subgraph's identifier on the decentralized network.
	ID string
	// Label names the protocol for a reader; it is not sent anywhere.
	Label string
}

// GraphConfig wires the provider.
type GraphConfig struct {
	APIKey     string
	GatewayURL string
	Subgraphs  []Subgraph
	Timeout    time.Duration
	Client     *http.Client
}

// DefaultSubgraphs are the venues the benchmark is taken across.
//
// Two protocols rather than one, because a median over a single venue is that venue's rate
// with extra steps, and a receivable is not priced against one lender's opinion.
func DefaultSubgraphs() []Subgraph {
	return []Subgraph{
		{ID: "JCNWRypm7FYwV8fx5HhzZPSFaMxgkPuw4TnR3Gpi81zk", Label: "aave-v3-ethereum"},
		{ID: "AwoxEZbiWLvv6e3QdvdMZw4WDURdGbvPfHmZRc8Dpfz9", Label: "compound-v3-ethereum"},
	}
}

// NewGraphProvider returns the live provider.
func NewGraphProvider(cfg GraphConfig) (*GraphProvider, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, apperr.Invalid("graph_api_key", "must not be empty")
	}
	if len(cfg.Subgraphs) == 0 {
		cfg.Subgraphs = DefaultSubgraphs()
	}
	if cfg.GatewayURL == "" {
		cfg.GatewayURL = DefaultGatewayURL
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout}
	}

	return &GraphProvider{
		endpoint:  strings.TrimRight(cfg.GatewayURL, "/"),
		apiKey:    cfg.APIKey,
		subgraphs: append([]Subgraph(nil), cfg.Subgraphs...),
		client:    cfg.Client,
	}, nil
}

// Subgraphs reports which sources this provider reads, for a startup log.
func (p *GraphProvider) Subgraphs() []string {
	labels := make([]string, 0, len(p.subgraphs))
	for _, subgraph := range p.subgraphs {
		labels = append(labels, subgraph.Label)
	}
	return labels
}

// graphResponse is the standardized lending schema, narrowed to what pricing uses.
type graphResponse struct {
	Data struct {
		Meta struct {
			Block struct {
				Number int64 `json:"number"`
			} `json:"block"`
		} `json:"_meta"`
		Markets []struct {
			ID                     string `json:"id"`
			Name                   string `json:"name"`
			TotalDepositBalanceUSD string `json:"totalDepositBalanceUSD"`
			Rates                  []struct {
				Side string `json:"side"`
				Type string `json:"type"`
				Rate string `json:"rate"`
			} `json:"rates"`
		} `json:"markets"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

/*
FetchMarkets asks every configured subgraph the same question.

A subgraph that fails takes the whole fetch with it. That is deliberate: a benchmark is a
median across venues, and silently dropping the venue that did not answer would move the
number without anything on the resulting snapshot saying so. Pricing fails closed instead,
and the invoice is assessed once the gateway is answering again.
*/
func (p *GraphProvider) FetchMarkets(ctx context.Context, q Query) ([]Market, error) {
	markets := make([]Market, 0, len(p.subgraphs)*4)

	for _, subgraph := range p.subgraphs {
		found, err := p.fetchOne(ctx, subgraph, q)
		if err != nil {
			return nil, fmt.Errorf("subgraph %s: %w", subgraph.Label, err)
		}
		markets = append(markets, found...)
	}

	if len(markets) == 0 {
		return nil, apperr.Unavailablef("no %s markets were returned by any subgraph", q.Asset)
	}
	return markets, nil
}

func (p *GraphProvider) fetchOne(ctx context.Context, subgraph Subgraph, q Query) ([]Market, error) {
	variables := make(map[string]any, len(q.Variables))
	for key, value := range q.Variables {
		variables[key] = value
	}

	body, err := json.Marshal(map[string]any{"query": q.GraphQL, "variables": variables})
	if err != nil {
		return nil, fmt.Errorf("encoding the query: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.endpoint+"/"+subgraph.ID, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// The key is a bearer token rather than a path segment, so it does not end up in a
	// gateway access log or a proxy's history.
	request.Header.Set("Authorization", "Bearer "+p.apiKey)
	request.Header.Set("Content-Type", "application/json")

	response, err := p.client.Do(request)
	if err != nil {
		return nil, apperr.Unavailablef("the gateway did not answer: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, apperr.Unavailablef("the gateway answered %s", response.Status)
	}

	var decoded graphResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decoding the answer: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return nil, apperr.Unavailablef("the gateway refused the query: %s", decoded.Errors[0].Message)
	}

	block := decoded.Data.Meta.Block.Number
	markets := make([]Market, 0, len(decoded.Data.Markets))

	for _, row := range decoded.Data.Markets {
		supply, ok := lenderRate(row.Rates)
		if !ok {
			// A market with no lender rate is not a place anyone can supply into, so it is
			// not a market this benchmark is about.
			continue
		}
		liquidity, err := usd(row.TotalDepositBalanceUSD)
		if err != nil {
			return nil, fmt.Errorf("market %s: %w", row.ID, err)
		}

		markets = append(markets, Market{
			ID:                 row.ID,
			SubgraphID:         subgraph.Label,
			Asset:              q.Asset,
			NetSupplyAPY:       supply,
			AvailableLiquidity: liquidity,
			BlockNumber:        block,
		})
	}
	return markets, nil
}

// lenderRate finds the variable supply rate and converts it from a percentage.
//
// The schema publishes 3.6274948769805519 for 3.63%, and the model works in decimals, so the
// point moves by two places. It moves by division on the exact decimal rather than by
// multiplying a float, for the same reason every other number here does.
func lenderRate(rates []struct {
	Side string `json:"side"`
	Type string `json:"type"`
	Rate string `json:"rate"`
}) (money.Rate, bool) {
	for _, rate := range rates {
		if !strings.EqualFold(rate.Side, "LENDER") || !strings.EqualFold(rate.Type, "VARIABLE") {
			continue
		}
		parsed, err := decimal.NewFromString(strings.TrimSpace(rate.Rate))
		if err != nil {
			return money.Rate{}, false
		}
		if parsed.IsNegative() {
			return money.Rate{}, false
		}
		// Shift moves the decimal point exactly; dividing would truncate at the library's
		// division precision and lose digits the gateway actually published.
		return money.NewRate(parsed.Shift(-2)), true
	}
	return money.Rate{}, false
}

// usd reads a deposit balance, which the schema publishes with more precision than money
// carries. Truncating to cents is right for a quantity used as a weight: the extra digits
// cannot change a median, and rounding them would invent value that is not there.
func usd(raw string) (money.Amount, error) {
	parsed, err := decimal.NewFromString(strings.TrimSpace(raw))
	if err != nil {
		return money.Amount{}, apperr.Invalid("total_deposit_balance_usd", "is not a number: %q", raw)
	}
	if parsed.IsNegative() {
		return money.Amount{}, apperr.Invalid("total_deposit_balance_usd", "must not be negative")
	}
	return money.Parse(parsed.Truncate(money.USD.Exponent()).String(), money.USD)
}

// GraphQuery is the question this provider asks, and the one hashed into every snapshot.
func GraphQuery(network, asset string) Query {
	return Query{
		Provider: "thegraph-gateway",
		Network:  network,
		Asset:    asset,
		GraphQL: `query Markets($asset: String!) {
			_meta { block { number } }
			markets(where: { inputToken_: { symbol: $asset } }) {
				id
				name
				totalDepositBalanceUSD
				rates { side type rate }
			}
		}`,
		Variables: map[string]string{"asset": asset},
	}
}
