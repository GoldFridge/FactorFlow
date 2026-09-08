package marketdata_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/marketdata"
	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/money"
)

// answer is one subgraph's reply, in the shape the standardized lending schema returns.
const answer = `{"data":{"_meta":{"block":{"number":25933271}},"markets":[
 {"id":"0xaave","name":"Aave Ethereum USDC","totalDepositBalanceUSD":"2307302128.55978035579935",
  "rates":[{"side":"BORROWER","type":"VARIABLE","rate":"4.2939051838591498"},
           {"side":"LENDER","type":"VARIABLE","rate":"3.6274948769805519"}]},
 {"id":"0xnorates","name":"deprecated","totalDepositBalanceUSD":"0.078","rates":[]}
]}}`

// call is what a subgraph saw, kept after the handler returned: a request's body is closed
// once the handler ends, so the bytes are read while they still exist.
type call struct {
	header http.Header
	url    string
	body   []byte
}

func graphServer(t *testing.T, body string, status int) (*httptest.Server, *[]call) {
	t.Helper()

	var seen []call
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, _ := io.ReadAll(r.Body)
		seen = append(seen, call{header: r.Header.Clone(), url: r.URL.String(), body: sent})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, &seen
}

func provider(t *testing.T, server *httptest.Server) *marketdata.GraphProvider {
	t.Helper()

	p, err := marketdata.NewGraphProvider(marketdata.GraphConfig{
		APIKey:     "test-key",
		GatewayURL: server.URL,
		Subgraphs:  []marketdata.Subgraph{{ID: "sg-1", Label: "aave-v3-ethereum"}},
	})
	require.NoError(t, err)
	return p
}

/*
 * The schema publishes a percentage and the model works in decimals, so this conversion is
 * the one thing here that could quietly move every price the platform publishes.
 */
func TestRatesArriveAsDecimalsNotPercentages(t *testing.T) {
	t.Parallel()

	server, _ := graphServer(t, answer, http.StatusOK)

	markets, err := provider(t, server).FetchMarkets(t.Context(), marketdata.GraphQuery("ethereum", "USDC"))
	require.NoError(t, err)
	require.Len(t, markets, 1, "a market with no lender rate is not somewhere anyone can supply")

	assert.Equal(t, "0.036274948769805519", markets[0].NetSupplyAPY.String(),
		"every digit the gateway published survives the conversion")
	assert.Equal(t, "2307302128.55", markets[0].AvailableLiquidity.String(),
		"a weight is truncated to cents rather than rounded up into value that is not there")
	assert.Equal(t, int64(25933271), markets[0].BlockNumber)
	assert.Equal(t, "aave-v3-ethereum", markets[0].SubgraphID)
	assert.Equal(t, "USDC", markets[0].Asset)
}

// The key travels as a bearer token so it does not end up in a gateway access log or a
// proxy's history, which a key in the path would.
func TestTheKeyIsSentAsABearerToken(t *testing.T) {
	t.Parallel()

	server, seen := graphServer(t, answer, http.StatusOK)
	_, err := provider(t, server).FetchMarkets(t.Context(), marketdata.GraphQuery("ethereum", "USDC"))
	require.NoError(t, err)

	require.Len(t, *seen, 1)
	request := (*seen)[0]
	assert.Equal(t, "Bearer test-key", request.header.Get("Authorization"))
	assert.Equal(t, "/sg-1", request.url)
	assert.NotContains(t, request.url, "test-key")
}

/*
 * A benchmark is a median across venues, so a venue that did not answer must not be dropped
 * quietly: the number would move and nothing on the resulting snapshot would say why.
 * Pricing fails closed instead.
 */
func TestOneFailingSubgraphFailsTheFetch(t *testing.T) {
	t.Parallel()

	broken, _ := graphServer(t, `{"errors":[{"message":"indexing error"}]}`, http.StatusOK)

	p, err := marketdata.NewGraphProvider(marketdata.GraphConfig{
		APIKey:     "test-key",
		GatewayURL: broken.URL,
		Subgraphs:  []marketdata.Subgraph{{ID: "sg-2", Label: "compound-v3-ethereum"}},
	})
	require.NoError(t, err)

	_, err = p.FetchMarkets(t.Context(), marketdata.GraphQuery("ethereum", "USDC"))
	require.ErrorIs(t, err, apperr.ErrUnavailable)
	assert.Contains(t, err.Error(), "compound-v3-ethereum")
}

func TestAGatewayFailureIsUnavailableRatherThanEmpty(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		body   string
		status int
	}{
		{name: "rejected", body: `{}`, status: http.StatusUnauthorized},
		{name: "no markets", body: `{"data":{"_meta":{"block":{"number":1}},"markets":[]}}`, status: http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := graphServer(t, tc.body, tc.status)
			_, err := provider(t, server).FetchMarkets(t.Context(), marketdata.GraphQuery("ethereum", "USDC"))
			assert.ErrorIs(t, err, apperr.ErrUnavailable)
		})
	}
}

func TestNewGraphProviderNeedsAKey(t *testing.T) {
	t.Parallel()

	_, err := marketdata.NewGraphProvider(marketdata.GraphConfig{})
	assert.ErrorIs(t, err, apperr.ErrValidation)
}

func TestTheQueryIsSentWithItsVariables(t *testing.T) {
	t.Parallel()

	server, seen := graphServer(t, answer, http.StatusOK)
	_, err := provider(t, server).FetchMarkets(t.Context(), marketdata.GraphQuery("ethereum", "USDC"))
	require.NoError(t, err)

	var sent struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables"`
	}
	require.NoError(t, json.Unmarshal((*seen)[0].body, &sent))
	assert.Contains(t, sent.Query, "totalDepositBalanceUSD")
	assert.Equal(t, map[string]string{"asset": "USDC"}, sent.Variables)
}

/*
 * TestAgainstTheLiveGateway is the only test here that proves anything about The Graph.
 *
 * The bound on the rate is the assertion that matters: a supply yield above 50 per cent means
 * the percentage conversion is wrong, not that a lending market has become generous.
 */
func TestAgainstTheLiveGateway(t *testing.T) {
	key := os.Getenv("FF_GRAPH_API_KEY")
	if testing.Short() || key == "" {
		t.Skip("network test: needs FF_GRAPH_API_KEY")
	}

	p, err := marketdata.NewGraphProvider(marketdata.GraphConfig{APIKey: key, Timeout: 30 * time.Second})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	markets, err := p.FetchMarkets(ctx, marketdata.GraphQuery("ethereum", "USDC"))
	require.NoError(t, err)
	require.NotEmpty(t, markets)

	half := mustRate(t, "0.5")
	sources := map[string]bool{}
	for _, market := range markets {
		assert.False(t, market.NetSupplyAPY.IsNegative())
		assert.Negative(t, market.NetSupplyAPY.Cmp(half))
		assert.Positive(t, market.BlockNumber)
		sources[market.SubgraphID] = true

		t.Logf("%s %s: %s at %s", market.SubgraphID, market.ID,
			market.NetSupplyAPY.StringFixed(6), market.AvailableLiquidity)
	}
	assert.Len(t, sources, 2, "the benchmark is taken across both venues")
}

func mustRate(t *testing.T, raw string) money.Rate {
	t.Helper()

	rate, err := money.ParseRate(raw)
	require.NoError(t, err)
	return rate
}
