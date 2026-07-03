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

func TestUpsertByThread_NewThenUpdate(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	// 1) New application → new row.
	r1, err := s.UpsertByThread(Application{
		Company: "Acme", Status: "applied",
		EmailMessageID: "msg-1", EmailThreadID: "thread-1",
		Subject: "Application received", AppliedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("upsert1: %v", err)
	}
	if !r1.Created {
		t.Fatalf("first record should be Created")
	}

	// 2) Same thread, new mail, status changed → update (do NOT insert a new row).
	r2, err := s.UpsertByThread(Application{
		Company: "Acme", Status: "interview",
		EmailMessageID: "msg-2", EmailThreadID: "thread-1",
		Subject: "Interview invite", AppliedAt: now, UpdatedAt: now.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	if r2.Created {
		t.Fatalf("same thread should not create a new row")
	}
	if !r2.StatusChanged || r2.OldStatus != "applied" {
		t.Fatalf("status change should be detected, got changed=%v old=%q", r2.StatusChanged, r2.OldStatus)
	}

	apps, err := s.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("expected 1 row, found %d", len(apps))
	}
	if apps[0].Status != "interview" {
		t.Fatalf("status should be interview, got %q", apps[0].Status)
	}
}

func TestUpsertByThread_DifferentThreadNewRow(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	_, _ = s.UpsertByThread(Application{
		Status: "applied", EmailMessageID: "m1", EmailThreadID: "t1", UpdatedAt: now,
	})
	r, _ := s.UpsertByThread(Application{
		Status: "applied", EmailMessageID: "m2", EmailThreadID: "t2", UpdatedAt: now,
	})
	if !r.Created {
		t.Fatalf("a different thread should create a new row")
	}
	apps, _ := s.List()
	if len(apps) != 2 {
		t.Fatalf("expected 2 rows, found %d", len(apps))
	}
}

func TestMessageExists(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	if ok, _ := s.MessageExists("m1"); ok {
		t.Fatalf("exists should be false initially")
	}
	_, _ = s.UpsertByThread(Application{
		Status: "applied", EmailMessageID: "m1", EmailThreadID: "t1", UpdatedAt: now,
	})
	if ok, _ := s.MessageExists("m1"); !ok {
		t.Fatalf("exists should be true after insert")
	}
}
