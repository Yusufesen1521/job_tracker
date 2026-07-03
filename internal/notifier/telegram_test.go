package notifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTelegramNotify(t *testing.T) {
	var gotChatID, gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("ParseForm: %v", err)
		}
		gotChatID = r.PostForm.Get("chat_id")
		gotText = r.PostForm.Get("text")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	tg := NewTelegram("test-token", "12345")
	tg.endpoint = srv.URL

	if err := tg.Notify(context.Background(), "New application: Acme", "Acme — status: applied"); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotChatID != "12345" {
		t.Fatalf("chat_id = %q, want 12345", gotChatID)
	}
	if gotText != "New application: Acme\nAcme — status: applied" {
		t.Fatalf("unexpected text: %q", gotText)
	}
}

func TestTelegramUnconfiguredIsNoop(t *testing.T) {
	tg := NewTelegram("", "")
	if err := tg.Notify(context.Background(), "t", "m"); err != nil {
		t.Fatalf("unconfigured telegram must be a no-op, got: %v", err)
	}
}
