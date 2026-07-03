package classifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// openRouterEndpoint is the OpenRouter chat completions address (OpenAI-compatible).
const openRouterEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// OpenRouter is the OpenRouter-based implementation of Classifier.
// Used as a fallback when the Gemini quota runs out (LLM_PROVIDER=openrouter).
type OpenRouter struct {
	apiKey   string
	model    string
	http     *http.Client
	endpoint string // overridable in tests; empty means the real API
}

// NewOpenRouter creates a new OpenRouter classifier.
// model e.g. "meta-llama/llama-3.3-70b-instruct:free" (free variant).
func NewOpenRouter(apiKey, model string) *OpenRouter {
	return &OpenRouter{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// --- API request/response types (OpenAI-compatible) ---

type openRouterRequest struct {
	Model     string              `json:"model"`
	MaxTokens int                 `json:"max_tokens"`
	Messages  []openRouterMessage `json:"messages"`
}

type openRouterMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openRouterResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Classify categorizes the mail via OpenRouter.
func (o *OpenRouter) Classify(ctx context.Context, email Email) (Result, error) {
	var res Result

	userMsg := fmt.Sprintf(userPromptTemplate, email.From, email.Subject, truncate(email.Body, 6000))

	reqBody := openRouterRequest{
		Model:     o.model,
		MaxTokens: 256,
		Messages: []openRouterMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userMsg},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return res, err
	}

	endpoint := o.endpoint
	if endpoint == "" {
		endpoint = openRouterEndpoint
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return res, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+o.apiKey)

	resp, err := o.http.Do(req)
	if err != nil {
		return res, fmt.Errorf("openrouter request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return res, fmt.Errorf("%w: %s", ErrQuotaExhausted, summarizeError(body))
	}
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("openrouter HTTP %d: %s", resp.StatusCode, string(body))
	}

	var or openRouterResponse
	if err := json.Unmarshal(body, &or); err != nil {
		return res, fmt.Errorf("could not parse openrouter response: %w", err)
	}
	if or.Error != nil {
		return res, fmt.Errorf("openrouter error: %s", or.Error.Message)
	}
	if len(or.Choices) == 0 {
		return res, fmt.Errorf("openrouter returned an empty response")
	}

	jsonStr := extractJSON(or.Choices[0].Message.Content)
	if jsonStr == "" {
		return res, fmt.Errorf("no JSON found in response: %q", or.Choices[0].Message.Content)
	}
	if err := json.Unmarshal([]byte(jsonStr), &res); err != nil {
		return res, fmt.Errorf("could not parse classification JSON: %w (raw: %s)", err, jsonStr)
	}
	return res, nil
}
