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

func newMockOpenRouter(t *testing.T, handler http.HandlerFunc) *OpenRouter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	o := NewOpenRouter("test-key", "test-model")
	o.endpoint = srv.URL
	return o
}

func TestOpenRouterClassify_Success(t *testing.T) {
	o := newMockOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("Authorization header missing/wrong")
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"{\"is_job_related\": true, \"company\": \"Acme\", \"status\": \"offer\", \"confidence\": 0.95}"}}]}`))
	})

	res, err := o.Classify(context.Background(), Email{Subject: "Offer letter"})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if res.Status != "offer" || res.Company != "Acme" {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestOpenRouterClassify_QuotaExhausted(t *testing.T) {
	o := newMockOpenRouter(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":429,"message":"Rate limit exceeded: free-models-per-day"}}`))
	})

	_, err := o.Classify(context.Background(), Email{Subject: "x"})
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got: %v", err)
	}
}

// TestOpenRouterLive sends one real request to the API; skipped by default.
//
//	LIVE_API_TEST=1 go test ./internal/classifier/ -run TestOpenRouterLive -v
func TestOpenRouterLive(t *testing.T) {
	if os.Getenv("LIVE_API_TEST") != "1" {
		t.Skip("live API test skipped (run with LIVE_API_TEST=1)")
	}
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("OPENROUTER_API_KEY not set")
	}
	model := os.Getenv("OPENROUTER_MODEL")
	if model == "" {
		model = "meta-llama/llama-3.3-70b-instruct:free"
	}

	o := NewOpenRouter(apiKey, model)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	res, err := o.Classify(ctx, Email{
		From:    "careers@example-corp.com",
		Subject: "Your application to Example Corp",
		Body:    "Thank you for applying to the Software Engineer position. We received your application.",
	})
	if errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("QUOTA EXHAUSTED (%s): %v", model, err)
	}
	if err != nil {
		t.Fatalf("API error: %v", err)
	}
	t.Logf("live result: %+v", res)
}
