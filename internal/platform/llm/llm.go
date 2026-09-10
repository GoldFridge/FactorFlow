// Package llm talks to an OpenAI-compatible chat completions endpoint.
//
// It is deliberately small and knows nothing about receivables. What it sends is what a
// caller hands it, which matters here more than usual: the privacy rule forbids document
// content in a prompt, and a package that assembled prompts from domain objects would be
// the place that quietly broke it. Assembling the question is the caller's job, and the
// caller is the one place where what may be said is decided.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultTimeout bounds one call. A narration is a nicety attached to a priced assessment,
// so it waits a short while and is then dropped rather than holding up the work.
const DefaultTimeout = 20 * time.Second

// MaxResponseBytes bounds what is read back, because a remote service is not trusted to be
// reasonable about size.
const MaxResponseBytes = 64 << 10

// Config points the client at a provider.
type Config struct {
	// BaseURL is the API root, without the /chat/completions suffix.
	BaseURL string
	APIKey  string
	Model   string
	Timeout time.Duration
}

// Client is a chat completions client.
type Client struct {
	http    *http.Client
	baseURL string
	apiKey  string
	model   string
}

// New returns a client, or nil when there are no credentials to use one with.
//
// Nil rather than an error: every provider in this system is optional, and a deployment
// without a key is a deployment that does without the narration, not a broken one.
func New(cfg Config) *Client {
	if strings.TrimSpace(cfg.APIKey) == "" || strings.TrimSpace(cfg.Model) == "" {
		return nil
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = "https://api.deepseek.com"
	}

	return &Client{
		http:    &http.Client{Timeout: cfg.Timeout},
		baseURL: base,
		apiKey:  strings.TrimSpace(cfg.APIKey),
		model:   strings.TrimSpace(cfg.Model),
	}
}

// Model reports which model this client speaks to, for the record kept beside an answer.
func (c *Client) Model() string { return c.model }

// Message is one turn of the conversation.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Roles a message can carry.
const (
	RoleSystem = "system"
	RoleUser   = "user"
)

type completionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	// Temperature is fixed by the caller rather than defaulted, because the same question
	// asked twice should not produce two different answers about the same price.
	Temperature float64 `json:"temperature"`
	MaxTokens   int     `json:"max_tokens"`
}

type completionResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// Complete asks the model and returns what it said.
func (c *Client) Complete(ctx context.Context, messages []Message, maxTokens int) (string, error) {
	if c == nil {
		return "", fmt.Errorf("llm: no model is configured")
	}
	if len(messages) == 0 {
		return "", fmt.Errorf("llm: nothing to ask")
	}
	if maxTokens <= 0 {
		maxTokens = 400
	}

	body, err := json.Marshal(completionRequest{
		Model:       c.model,
		Messages:    messages,
		Temperature: 0,
		MaxTokens:   maxTokens,
	})
	if err != nil {
		return "", fmt.Errorf("llm: encoding the request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm: building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	res, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: calling %s: %w", c.baseURL, err)
	}
	defer func() { _ = res.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("llm: reading the answer: %w", err)
	}

	var parsed completionResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// The body is not echoed: a provider's error page is not something to paste into a
		// log that a document's owner might read.
		return "", fmt.Errorf("llm: the answer was not JSON (status %d)", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		if parsed.Error != nil {
			return "", fmt.Errorf("llm: refused with %d: %s", res.StatusCode, parsed.Error.Message)
		}
		return "", fmt.Errorf("llm: refused with %d", res.StatusCode)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("llm: the model returned no answer")
	}

	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}
