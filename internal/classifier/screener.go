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

// screenerSystemPrompt is the pre-screening instruction: asks for a single
// YES/NO word. The screener does not need the main classifier's precision;
// it is told to answer YES when in doubt, because YES only means "worth
// asking the main classifier".
const screenerSystemPrompt = `You are an email pre-screening filter. You will be given an email's
sender, subject and a short excerpt. Decide whether this email COULD be related to a job
application process (application confirmations, rejections, interview invitations, job offers,
recruiter messages about the user's own application). Emails may be in Turkish or English.

Not job-application related: newsletters, promotions, job ads/listings the user has not applied
to, social media notifications, bills, receipts, general announcements.

Answer with EXACTLY one word: YES or NO. If in doubt, answer YES.`

// Screener is the second-stage pre-filter: for mails the keyword filter
// missed, it asks a cheap/free small LLM "could this be job related?".
// Uses the OpenRouter (OpenAI-compatible) API.
type Screener struct {
	apiKey   string
	model    string
	http     *http.Client
	endpoint string // overridable in tests; empty means the real API
}

// NewScreener creates a new LLM pre-screener.
// model e.g. "openai/gpt-oss-20b:free".
func NewScreener(apiKey, model string) *Screener {
	return &Screener{
		apiKey: apiKey,
		model:  model,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// IsJobRelated asks the small LLM about the mail and converts its YES/NO
// answer to a bool. Returns ErrQuotaExhausted when the quota is exhausted;
// the caller may disable the screener for the rest of the cycle.
func (s *Screener) IsJobRelated(ctx context.Context, email Email) (bool, error) {
	// A short excerpt is enough for pre-screening; don't waste tokens.
	userMsg := fmt.Sprintf(userPromptTemplate, email.From, email.Subject, truncate(email.Body, 800))

	// 64 tokens: the answer is one word, but reasoning models (e.g. gpt-oss)
	// also spend thinking tokens; a tight limit could leave the answer empty.
	reqBody := openRouterRequest{
		Model:     s.model,
		MaxTokens: 64,
		Messages: []openRouterMessage{
			{Role: "system", Content: screenerSystemPrompt},
			{Role: "user", Content: userMsg},
		},
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return false, err
	}

	endpoint := s.endpoint
	if endpoint == "" {
		endpoint = openRouterEndpoint
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return false, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+s.apiKey)

	resp, err := s.http.Do(req)
	if err != nil {
		return false, fmt.Errorf("screener request failed: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		return false, fmt.Errorf("%w: %s", ErrQuotaExhausted, summarizeError(body))
	}
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("screener HTTP %d: %s", resp.StatusCode, string(body))
	}

	var or openRouterResponse
	if err := json.Unmarshal(body, &or); err != nil {
		return false, fmt.Errorf("could not parse screener response: %w", err)
	}
	if or.Error != nil {
		return false, fmt.Errorf("screener error: %s", or.Error.Message)
	}
	if len(or.Choices) == 0 {
		return false, fmt.Errorf("screener returned an empty response")
	}

	answer := strings.ToUpper(strings.TrimSpace(or.Choices[0].Message.Content))
	return strings.HasPrefix(answer, "YES"), nil
}
