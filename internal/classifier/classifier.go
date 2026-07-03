// Package classifier categorizes mails by job-application status.
// Classifier is an interface; implementations exist for Gemini, OpenRouter
// and Anthropic, and it can be mocked in tests.
package classifier

import "context"

// Email is the simplified form of a mail sent to the LLM for classification.
type Email struct {
	From    string
	Subject string
	Body    string // plain-text body or snippet
}

// Result is the structured classification returned by the LLM.
type Result struct {
	IsJobRelated bool    `json:"is_job_related"`
	Company      string  `json:"company"`  // the actual employer, not the sending platform
	Position     string  `json:"position"` // "" when the mail never names it
	Via          string  `json:"via"`      // intermediary platform/agency ("" = direct)
	Status       string  `json:"status"`   // applied | rejected | interview | offer
	Confidence   float64 `json:"confidence"`
}

// Classifier is the mail-classification abstraction.
type Classifier interface {
	Classify(ctx context.Context, email Email) (Result, error)
}
