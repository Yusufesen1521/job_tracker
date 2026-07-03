package classifier

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newMockScreener(t *testing.T, handler http.HandlerFunc) *Screener {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	s := NewScreener("test-key", "test-model")
	s.endpoint = srv.URL
	return s
}

func TestScreenerYesNo(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    bool
	}{
		{"plain yes", "YES", true},
		{"lowercase yes", "yes", true},
		{"yes with trailing text", "YES, this looks job-related.", true},
		{"plain no", "NO", false},
		{"no with punctuation", "No.", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newMockScreener(t, func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(`{"choices":[{"message":{"content":"` + tt.content + `"}}]}`))
			})
			got, err := s.IsJobRelated(context.Background(), Email{Subject: "x"})
			if err != nil {
				t.Fatalf("IsJobRelated: %v", err)
			}
			if got != tt.want {
				t.Fatalf("for answer %q expected %v, got %v", tt.content, tt.want, got)
			}
		})
	}
}

func TestScreenerQuotaExhausted(t *testing.T) {
	s := newMockScreener(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error":{"code":429,"message":"free-models-per-day"}}`))
	})
	_, err := s.IsJobRelated(context.Background(), Email{Subject: "x"})
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got: %v", err)
	}
}
