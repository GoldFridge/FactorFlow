package llm_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoldFridge/factorflow/internal/platform/llm"
)

// TestAskingTheModel covers the shape of the exchange: what is sent, and what is read back.
func TestAskingTheModel(t *testing.T) {
	t.Parallel()

	var sent map[string]any
	var auth string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/chat/completions", r.URL.Path)
		auth = r.Header.Get("Authorization")

		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &sent))

		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":" two lines "}}]}`))
	}))
	defer server.Close()

	client := llm.New(llm.Config{BaseURL: server.URL, APIKey: "key", Model: "test-model"})
	require.NotNil(t, client)

	answer, err := client.Complete(context.Background(), []llm.Message{
		{Role: llm.RoleSystem, Content: "be brief"},
		{Role: llm.RoleUser, Content: "why this price"},
	}, 100)
	require.NoError(t, err)

	assert.Equal(t, "two lines", answer, "the answer comes back trimmed")
	assert.Equal(t, "Bearer key", auth)
	assert.Equal(t, "test-model", sent["model"])
	assert.InDelta(t, 0.0, sent["temperature"], 0,
		"the same question about the same price must not get two different answers")
}

/*
 * TestNoModelIsNotAFailure. Every provider here is optional: a deployment without a key
 * does without the narration rather than being broken by its absence.
 */
func TestNoModelIsNotAFailure(t *testing.T) {
	t.Parallel()

	assert.Nil(t, llm.New(llm.Config{Model: "test-model"}), "no key, no client")
	assert.Nil(t, llm.New(llm.Config{APIKey: "key"}), "no model, no client")

	var absent *llm.Client
	_, err := absent.Complete(context.Background(), []llm.Message{{Role: llm.RoleUser}}, 10)
	require.Error(t, err, "and calling the one that does not exist says so plainly")
}

func TestARefusalIsReported(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit reached","type":"rate_limit"}}`))
	}))
	defer server.Close()

	client := llm.New(llm.Config{BaseURL: server.URL, APIKey: "key", Model: "test-model"})
	_, err := client.Complete(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "x"}}, 10)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limit reached")
}

/*
 * TestAnErrorPageIsNotPastedIntoTheLog. A provider that answers with HTML is a normal
 * failure; repeating its body into an error message is how something a document's owner
 * might read ends up somewhere it should not.
 */
func TestAnErrorPageIsNotPastedIntoTheLog(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>upstream said something private</body></html>"))
	}))
	defer server.Close()

	client := llm.New(llm.Config{BaseURL: server.URL, APIKey: "key", Model: "test-model"})
	_, err := client.Complete(context.Background(), []llm.Message{{Role: llm.RoleUser, Content: "x"}}, 10)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "upstream said something private")
	assert.Contains(t, err.Error(), "502")
}
