package classifier

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

// anthropicEndpoint is the Anthropic Messages API address.
const anthropicEndpoint = "https://api.anthropic.com/v1/messages"

// anthropicVersion is the API version header (required by Anthropic).
const anthropicVersion = "2023-06-01"

// Anthropic is the Claude-based implementation of Classifier.
// Calls are made with net/http, no external SDK (minimal dependencies).
type Anthropic struct {
	apiKey string
	model  string
	http   *http.Client
}

// NewAnthropic creates a new Anthropic classifier.
func NewAnthropic(apiKey, model string) *Anthropic {
	return &Anthropic{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// --- API request/response types ---

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	System    string             `json:"system"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// Classify sends the mail to Claude and parses the structured JSON result.
func (a *Anthropic) Classify(ctx context.Context, email Email) (Result, error) {
	var res Result

	userMsg := fmt.Sprintf(userPromptTemplate, email.From, email.Subject, truncate(email.Body, 6000))

	reqBody := anthropicRequest{
		Model:     a.model,
		MaxTokens: 256,
		System:    systemPrompt,
		Messages:  []anthropicMessage{{Role: "user", Content: userMsg}},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return res, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicEndpoint, bytes.NewReader(payload))
	if err != nil {
		return res, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := a.http.Do(req)
	if err != nil {
		return res, fmt.Errorf("anthropic request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return res, fmt.Errorf("%w: %s", ErrQuotaExhausted, summarizeError(body))
	}
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("anthropic HTTP %d: %s", resp.StatusCode, string(body))
	}

	var ar anthropicResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return res, fmt.Errorf("could not parse anthropic response: %w", err)
	}
	if ar.Error != nil {
		return res, fmt.Errorf("anthropic error: %s", ar.Error.Message)
	}

	// Take the first text block and extract the JSON inside.
	var text string
	for _, c := range ar.Content {
		if c.Type == "text" {
			text += c.Text
		}
	}
	jsonStr := extractJSON(text)
	if jsonStr == "" {
		return res, fmt.Errorf("no JSON found in response: %q", text)
	}
	if err := json.Unmarshal([]byte(jsonStr), &res); err != nil {
		return res, fmt.Errorf("could not parse classification JSON: %w (raw: %s)", err, jsonStr)
	}
	return res, nil
}

// extractJSON pulls the first { ... } JSON object out of the text.
// The model may wrap the JSON in a ```json block; that case is tolerated.
func extractJSON(s string) string {
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start == -1 || end == -1 || end < start {
		return ""
	}
	return s[start : end+1]
}

// truncate cuts long bodies to avoid wasting tokens.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
