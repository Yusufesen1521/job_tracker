package classifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// geminiEndpointTemplate is the Google Generative Language API address. %s: model name.
const geminiEndpointTemplate = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"

// ErrQuotaExhausted signals that the API quota (RPM/RPD) is exhausted.
// The --check command and the poller report this error specially.
var ErrQuotaExhausted = errors.New("API quota exhausted (HTTP 429)")

// Gemini is the Google Gemini implementation of Classifier.
// Calls are made with net/http, no external SDK (minimal dependencies).
type Gemini struct {
	apiKey   string
	model    string
	http     *http.Client
	endpoint string // overridable in tests; empty means the real API
}

// NewGemini creates a new Gemini classifier.
// model e.g. "gemini-3.1-flash-lite" (free tier: 500 requests/day).
func NewGemini(apiKey, model string) *Gemini {
	return &Gemini{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// --- API request/response types ---

type geminiRequest struct {
	SystemInstruction *geminiContent  `json:"system_instruction,omitempty"`
	Contents          []geminiContent `json:"contents"`
	GenerationConfig  geminiGenConfig `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiGenConfig struct {
	MaxOutputTokens  int    `json:"maxOutputTokens"`
	ResponseMimeType string `json:"responseMimeType"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
	} `json:"error"`
}

// Classify sends the mail to Gemini and parses the structured JSON result.
// responseMimeType=application/json forces the model to return pure JSON.
//
// On per-minute (RPM) limit hits it waits the API-suggested delay and retries
// a few times; when the daily quota (RPD) is exhausted it returns ErrQuotaExhausted.
func (g *Gemini) Classify(ctx context.Context, email Email) (Result, error) {
	var res Result
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var retryAfter time.Duration
		res, retryAfter, err = g.classifyOnce(ctx, email)
		// Only retry short-lived (RPM) 429s.
		if errors.Is(err, ErrQuotaExhausted) && retryAfter > 0 && retryAfter <= 90*time.Second {
			select {
			case <-time.After(retryAfter + time.Second):
				continue
			case <-ctx.Done():
				return res, ctx.Err()
			}
		}
		return res, err
	}
	return res, err
}

func (g *Gemini) classifyOnce(ctx context.Context, email Email) (Result, time.Duration, error) {
	var res Result

	userMsg := fmt.Sprintf(userPromptTemplate, email.From, email.Subject, truncate(email.Body, 6000))

	reqBody := geminiRequest{
		SystemInstruction: &geminiContent{Parts: []geminiPart{{Text: systemPrompt}}},
		Contents:          []geminiContent{{Role: "user", Parts: []geminiPart{{Text: userMsg}}}},
		GenerationConfig: geminiGenConfig{
			MaxOutputTokens:  256,
			ResponseMimeType: "application/json",
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return res, 0, err
	}

	endpoint := g.endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf(geminiEndpointTemplate, g.model)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return res, 0, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.http.Do(req)
	if err != nil {
		return res, 0, fmt.Errorf("gemini request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return res, parseRetryDelay(body), fmt.Errorf("%w: %s", ErrQuotaExhausted, summarizeError(body))
	}
	if resp.StatusCode != http.StatusOK {
		return res, 0, fmt.Errorf("gemini HTTP %d: %s", resp.StatusCode, string(body))
	}

	var gr geminiResponse
	if err := json.Unmarshal(body, &gr); err != nil {
		return res, 0, fmt.Errorf("could not parse gemini response: %w", err)
	}
	if gr.Error != nil {
		return res, 0, fmt.Errorf("gemini error: %s", gr.Error.Message)
	}
	if len(gr.Candidates) == 0 {
		return res, 0, fmt.Errorf("gemini returned an empty response")
	}

	var text string
	for _, p := range gr.Candidates[0].Content.Parts {
		text += p.Text
	}
	jsonStr := extractJSON(text)
	if jsonStr == "" {
		return res, 0, fmt.Errorf("no JSON found in response: %q", text)
	}
	if err := json.Unmarshal([]byte(jsonStr), &res); err != nil {
		return res, 0, fmt.Errorf("could not parse classification JSON: %w (raw: %s)", err, jsonStr)
	}
	return res, 0, nil
}

// parseRetryDelay extracts the suggested wait time from the RetryInfo detail
// in a 429 body. RPM (per-minute) violations come with a short delay; if none
// is found it returns 0 (assumed daily quota, no retry).
func parseRetryDelay(body []byte) time.Duration {
	var e struct {
		Error struct {
			Details []struct {
				Type       string `json:"@type"`
				RetryDelay string `json:"retryDelay"`
			} `json:"details"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return 0
	}
	for _, d := range e.Error.Details {
		if d.RetryDelay == "" {
			continue
		}
		if dur, err := time.ParseDuration(d.RetryDelay); err == nil {
			return dur
		}
	}
	return 0
}

// summarizeError briefly extracts the API error message from a 429 body.
func summarizeError(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	s := string(body)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
