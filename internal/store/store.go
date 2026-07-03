// Package store provides the SQLite schema and the repository for the
// applications table. Uses a pure-Go driver (modernc.org/sqlite), no CGO needed.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Application is one job application record (a row in the applications table).
type Application struct {
	ID                int64
	Company           string
	Status            string // applied | rejected | interview | offer
	EmailMessageID    string // unique, used for dedup
	EmailThreadID     string
	Subject           string
	AppliedAt         time.Time
	UpdatedAt         time.Time
	RawClassification string // JSON, kept for debugging
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens the SQLite database at the given path and prepares the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite: %w", err)
	}
	// The modernc driver is safer with a single connection.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error { return s.db.Close() }

// init creates the schema (idempotent).
func (s *Store) init() error {
	const schema = `
CREATE TABLE IF NOT EXISTS applications (
	id                  INTEGER PRIMARY KEY AUTOINCREMENT,
	company             TEXT NOT NULL DEFAULT '',
	status              TEXT NOT NULL DEFAULT '',
	email_message_id    TEXT NOT NULL UNIQUE,
	email_thread_id     TEXT NOT NULL DEFAULT '',
	subject             TEXT NOT NULL DEFAULT '',
	applied_at          TIMESTAMP,
	updated_at          TIMESTAMP,
	raw_classification  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_applications_thread ON applications(email_thread_id);
CREATE INDEX IF NOT EXISTS idx_applications_message ON applications(email_message_id);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}
	return nil
}

// MessageExists reports whether this mail was already processed (dedup check).
func (s *Store) MessageExists(messageID string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM applications WHERE email_message_id = ?`, messageID,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// UpsertResult summarizes the outcome of UpsertByThread.
type UpsertResult struct {
	Created       bool   // a new row was inserted
	StatusChanged bool   // an existing row's status changed
	OldStatus     string // status before the change (meaningful if StatusChanged)
}

// UpsertByThread inserts or updates a record using thread semantics:
//   - If a row with the same thread_id exists, that row is updated (no new row).
//   - If the thread is unknown, a new application row is inserted.
//
// StatusChanged=true is returned only when the status actually changed
// (used as the notifier trigger).
func (s *Store) UpsertByThread(app Application) (UpsertResult, error) {
	var res UpsertResult

	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	// Is there an existing record from the same thread?
	// (Skip thread matching when thread_id is empty.)
	var (
		existingID     int64
		existingStatus string
	)
	if app.EmailThreadID != "" {
		err = tx.QueryRow(
			`SELECT id, status FROM applications WHERE email_thread_id = ? ORDER BY id LIMIT 1`,
			app.EmailThreadID,
		).Scan(&existingID, &existingStatus)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return res, err
		}
	} else {
		err = sql.ErrNoRows
	}

	if errors.Is(err, sql.ErrNoRows) || existingID == 0 {
		// New application → new row.
		_, err = tx.Exec(
			`INSERT INTO applications
				(company, status, email_message_id, email_thread_id, subject, applied_at, updated_at, raw_classification)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			app.Company, app.Status, app.EmailMessageID, app.EmailThreadID,
			app.Subject, app.AppliedAt, app.UpdatedAt, app.RawClassification,
		)
		if err != nil {
			return res, fmt.Errorf("insert failed: %w", err)
		}
		res.Created = true
	} else {
		// Existing thread → update.
		res.StatusChanged = existingStatus != app.Status
		res.OldStatus = existingStatus
		_, err = tx.Exec(
			`UPDATE applications
			    SET status = ?, subject = ?, updated_at = ?, raw_classification = ?,
			        email_message_id = ?, company = COALESCE(NULLIF(?, ''), company)
			  WHERE id = ?`,
			app.Status, app.Subject, app.UpdatedAt, app.RawClassification,
			app.EmailMessageID, app.Company, existingID,
		)
		if err != nil {
			return res, fmt.Errorf("update failed: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return res, err
	}
	return res, nil
}

// List returns all applications ordered by last update, descending (web UI).
func (s *Store) List() ([]Application, error) {
	rows, err := s.db.Query(
		`SELECT id, company, status, email_message_id, email_thread_id, subject,
		        applied_at, updated_at, raw_classification
		   FROM applications
		  ORDER BY updated_at DESC, id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Application
	for rows.Next() {
		var a Application
		var applied, updated sql.NullTime
		if err := rows.Scan(
			&a.ID, &a.Company, &a.Status, &a.EmailMessageID, &a.EmailThreadID,
			&a.Subject, &applied, &updated, &a.RawClassification,
		); err != nil {
			return nil, err
		}
		if applied.Valid {
			a.AppliedAt = applied.Time
		}
		if updated.Valid {
			a.UpdatedAt = updated.Time
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
