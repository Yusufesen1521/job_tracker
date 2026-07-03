package store

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	// File-based temp DB (modernc :memory: can misbehave with a single connection).
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func email(msgID, threadID, subject string, at time.Time) Email {
	return Email{
		GmailMessageID: msgID,
		GmailThreadID:  threadID,
		Subject:        subject,
		ReceivedAt:     at,
	}
}

func TestRecordEmail_ThreadMatch(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	r1, err := s.RecordEmail("Acme", "", "", "applied", email("m1", "t1", "Application received", now))
	if err != nil {
		t.Fatalf("record1: %v", err)
	}
	if !r1.Created {
		t.Fatalf("first record should create an application")
	}

	// Same thread, new mail, status forward → update, no new application.
	r2, err := s.RecordEmail("Acme", "", "", "interview", email("m2", "t1", "Interview invite", now.Add(time.Hour)))
	if err != nil {
		t.Fatalf("record2: %v", err)
	}
	if r2.Created {
		t.Fatalf("same thread must not create a new application")
	}
	if !r2.StatusChanged || r2.OldStatus != "applied" {
		t.Fatalf("status change should be detected, got changed=%v old=%q", r2.StatusChanged, r2.OldStatus)
	}

	apps := list(t, s)
	if len(apps) != 1 || apps[0].Status != "interview" || len(apps[0].Emails) != 2 {
		t.Fatalf("expected 1 app (interview) with 2 emails, got %+v", apps)
	}
}

func TestRecordEmail_CompanyMergeAcrossThreads(t *testing.T) {
	// LinkedIn confirmation and the company's own confirmation arrive in
	// different threads but must merge into one application.
	s := newTestStore(t)
	now := time.Now().UTC()

	_, _ = s.RecordEmail("Lucida AI", "", "LinkedIn", "applied", email("m1", "t1", "başvurunuz gönderildi", now))
	r2, err := s.RecordEmail("Lucida AI", "", "", "applied", email("m2", "t2", "Thanks for applying", now.Add(time.Minute)))
	if err != nil {
		t.Fatalf("record2: %v", err)
	}
	if r2.Created {
		t.Fatalf("same company should merge, not create a second application")
	}
	if r2.StatusChanged {
		t.Fatalf("applied→applied must not report a status change")
	}

	apps := list(t, s)
	if len(apps) != 1 || len(apps[0].Emails) != 2 {
		t.Fatalf("expected 1 application with 2 emails, got %d apps", len(apps))
	}
	if apps[0].Via != "LinkedIn" {
		t.Fatalf("via should be kept from the first mail, got %q", apps[0].Via)
	}
}

func TestRecordEmail_RejectedIsTerminal(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	_, _ = s.RecordEmail("AppNation", "", "", "applied", email("m1", "t1", "received", now))
	r2, _ := s.RecordEmail("AppNation", "", "", "rejected", email("m2", "t2", "update", now.Add(time.Hour)))
	if !r2.StatusChanged || r2.OldStatus != "applied" {
		t.Fatalf("applied→rejected should change status")
	}

	// A late duplicate confirmation must not downgrade rejected.
	r3, _ := s.RecordEmail("AppNation", "", "", "applied", email("m3", "t3", "late confirmation", now.Add(2*time.Hour)))
	if r3.Created || r3.StatusChanged {
		t.Fatalf("late applied mail must neither create nor change status, got %+v", r3)
	}

	apps := list(t, s)
	if len(apps) != 1 || apps[0].Status != "rejected" {
		t.Fatalf("expected single rejected application, got %+v", apps)
	}
	if len(apps[0].Emails) != 3 {
		t.Fatalf("all 3 mails should be recorded in history, got %d", len(apps[0].Emails))
	}
}

func TestRecordEmail_PositionSeparatesApplications(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	_, _ = s.RecordEmail("Vertigo", "Backend Developer", "", "applied", email("m1", "t1", "x", now))
	r2, _ := s.RecordEmail("Vertigo", "Game Developer", "", "applied", email("m2", "t2", "y", now))
	if !r2.Created {
		t.Fatalf("a different position should create a separate application")
	}

	// A rejection naming one position only touches that application.
	r3, _ := s.RecordEmail("Vertigo", "Backend Developer", "", "rejected", email("m3", "t3", "z", now.Add(time.Hour)))
	if r3.Created || !r3.StatusChanged {
		t.Fatalf("rejection should update the matching position, got %+v", r3)
	}

	apps := list(t, s)
	if len(apps) != 2 {
		t.Fatalf("expected 2 applications, got %d", len(apps))
	}
}

func TestRecordEmail_PositionedMailCompletesPositionless(t *testing.T) {
	// A LinkedIn confirmation without a position, then a rejection naming it:
	// they must merge and the position must be filled in.
	s := newTestStore(t)
	now := time.Now().UTC()

	_, _ = s.RecordEmail("Peak", "", "LinkedIn", "applied", email("m1", "t1", "x", now))
	r2, _ := s.RecordEmail("Peak", "Software Engineer", "", "rejected", email("m2", "t2", "y", now.Add(time.Hour)))
	if r2.Created {
		t.Fatalf("positioned mail should complete the positionless application")
	}

	apps := list(t, s)
	if len(apps) != 1 || apps[0].Position != "Software Engineer" || apps[0].Status != "rejected" {
		t.Fatalf("expected merged rejected application with position filled, got %+v", apps[0])
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Acme Inc.", "acme"},
		{"GELECEK VARLIK YÖNETİMİ A.Ş", "gelecek varlik yönetimi"},
		{"Azberry BV", "azberry"},
		{"Midas Games", "midas games"}, // "games" is not a legal suffix; must stay
		{"  Trendyol  ", "trendyol"},
	}
	for _, tt := range tests {
		if got := Normalize(tt.in); got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestMessageExists(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	if ok, _ := s.MessageExists("m1"); ok {
		t.Fatalf("exists should be false initially")
	}
	_, _ = s.RecordEmail("Acme", "", "", "applied", email("m1", "t1", "x", now))
	if ok, _ := s.MessageExists("m1"); !ok {
		t.Fatalf("exists should be true after insert")
	}
}

func TestMeta(t *testing.T) {
	s := newTestStore(t)
	if v, _ := s.GetMeta("last_history_id"); v != "" {
		t.Fatalf("empty meta should return \"\"")
	}
	_ = s.SetMeta("last_history_id", "12345")
	_ = s.SetMeta("last_history_id", "67890") // upsert
	if v, _ := s.GetMeta("last_history_id"); v != "67890" {
		t.Fatalf("meta upsert failed, got %q", v)
	}
}

func list(t *testing.T, s *Store) []Application {
	t.Helper()
	apps, err := s.ListWithEmails()
	if err != nil {
		t.Fatalf("ListWithEmails: %v", err)
	}
	return apps
}
