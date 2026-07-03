package classifier

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// newMockGemini builds a Gemini classifier that talks to the given handler.
func newMockGemini(t *testing.T, handler http.HandlerFunc) *Gemini {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	g := NewGemini("test-key", "test-model")
	g.endpoint = srv.URL
	return g
}

func TestGeminiClassify_Success(t *testing.T) {
	g := newMockGemini(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") != "test-key" {
			t.Errorf("API key header missing/wrong")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"is_job_related\": true, \"company\": \"Acme\", \"status\": \"interview\", \"confidence\": 0.9}"}]}}]}`))
	})

	res, err := g.Classify(context.Background(), Email{From: "hr@acme.com", Subject: "Interview", Body: "..."})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if !res.IsJobRelated || res.Company != "Acme" || res.Status != "interview" || res.Confidence != 0.9 {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestGeminiClassify_QuotaExhausted(t *testing.T) {
	g := newMockGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Quota exceeded for quota metric"}}`))
	})

	_, err := g.Classify(context.Background(), Email{Subject: "x"})
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got: %v", err)
	}
}

func TestGeminiClassify_RPMRetry(t *testing.T) {
	// First request returns an RPM 429 (with a short retryDelay), second succeeds.
	var calls int
	g := newMockGemini(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"rpm","details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.05s"}]}}`))
			return
		}
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"is_job_related\": true, \"company\": \"Acme\", \"status\": \"applied\", \"confidence\": 0.8}"}]}}]}`))
	})

	res, err := g.Classify(context.Background(), Email{Subject: "x"})
	if err != nil {
		t.Fatalf("retry after an RPM 429 should have succeeded: %v", err)
	}
	if calls != 2 || res.Company != "Acme" {
		t.Fatalf("expected 2 calls (got %d), result: %+v", calls, res)
	}
}

func TestGeminiClassify_MarkdownWrappedJSON(t *testing.T) {
	// The model may return a ```json block despite responseMimeType; tolerate it.
	g := newMockGemini(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"` + "```json\\n{\\\"is_job_related\\\": false, \\\"company\\\": \\\"\\\", \\\"status\\\": \\\"applied\\\", \\\"confidence\\\": 0.1}\\n```" + `"}]}}]}`))
	})

	res, err := g.Classify(context.Background(), Email{Subject: "spam"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.IsJobRelated {
		t.Fatalf("expected is_job_related=false")
	}
}

// TestGeminiLive sends one real request to the API and reports quota status.
// Skipped by default so tests don't consume quota; run it with:
//
//	LIVE_API_TEST=1 go test ./internal/classifier/ -run TestGeminiLive -v
//
// If the quota is exhausted the test FAILS with a clear message — this is how
// you learn the limit is gone before it matters (or use: go run ./cmd/poller --check).
func TestGeminiLive(t *testing.T) {
	if os.Getenv("LIVE_API_TEST") != "1" {
		t.Skip("live API test skipped (run with LIVE_API_TEST=1)")
	}
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		t.Skip("GEMINI_API_KEY not set")
	}
	model := os.Getenv("GEMINI_MODEL")
	if model == "" {
		model = "gemini-3.1-flash-lite"
	}

	g := NewGemini(apiKey, model)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := g.Classify(ctx, Email{
		From:    "careers@example-corp.com",
		Subject: "Your application to Example Corp",
		Body:    "Thank you for applying to the Software Engineer position. We received your application.",
	})
	if errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("QUOTA EXHAUSTED (%s) — consider switching to LLM_PROVIDER=openrouter: %v", model, err)
	}
	if err != nil {
		t.Fatalf("API error: %v", err)
	}
	t.Logf("live result: %+v", res)
	if !res.IsJobRelated {
		t.Errorf("the sample application mail should have is_job_related=true")
	}
}
