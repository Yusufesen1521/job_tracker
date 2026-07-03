// Package store provides the SQLite schema and the repositories for the
// applications and application_emails tables. Uses a pure-Go driver
// (modernc.org/sqlite), no CGO needed.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// Application is one job application (a row in the applications table).
// A single application aggregates every related email (confirmation from a
// job board, confirmation from the company itself, rejection, etc.) so the
// UI shows one row per company+position with its current status.
type Application struct {
	ID           int64
	Company      string
	Position     string // "" when the mails never named the position
	Via          string // intermediary platform/agency ("" = direct)
	Status       string // current status: applied | interview | offer | rejected
	FirstEmailAt time.Time
	LastEmailAt  time.Time
	Emails       []Email // populated by ListWithEmails
}

// Email is one classified mail belonging to an application.
type Email struct {
	ID                int64
	ApplicationID     int64
	GmailMessageID    string
	GmailThreadID     string
	FromAddr          string
	Subject           string
	Status            string // this mail's own classification
	RawClassification string
	ReceivedAt        time.Time
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
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	company        TEXT NOT NULL DEFAULT '',
	company_norm   TEXT NOT NULL DEFAULT '',
	position       TEXT NOT NULL DEFAULT '',
	position_norm  TEXT NOT NULL DEFAULT '',
	via            TEXT NOT NULL DEFAULT '',
	status         TEXT NOT NULL DEFAULT '',
	first_email_at TIMESTAMP,
	last_email_at  TIMESTAMP,
	created_at     TIMESTAMP,
	updated_at     TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_applications_company ON applications(company_norm);

CREATE TABLE IF NOT EXISTS application_emails (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	application_id     INTEGER NOT NULL REFERENCES applications(id),
	gmail_message_id   TEXT NOT NULL UNIQUE,
	gmail_thread_id    TEXT NOT NULL DEFAULT '',
	from_addr          TEXT NOT NULL DEFAULT '',
	subject            TEXT NOT NULL DEFAULT '',
	status             TEXT NOT NULL DEFAULT '',
	raw_classification TEXT NOT NULL DEFAULT '',
	received_at        TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_emails_application ON application_emails(application_id);
CREATE INDEX IF NOT EXISTS idx_emails_thread ON application_emails(gmail_thread_id);

CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT ''
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("failed to create schema: %w", err)
	}
	return nil
}

// --- meta (key/value; stores e.g. the Gmail last_history_id) ---

// GetMeta returns the value for key, or "" when absent.
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta upserts a key/value pair.
func (s *Store) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// MessageExists reports whether this mail was already processed (dedup check).
func (s *Store) MessageExists(messageID string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(1) FROM application_emails WHERE gmail_message_id = ?`, messageID,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RecordResult summarizes the outcome of RecordEmail.
type RecordResult struct {
	Created       bool   // a new application was created
	StatusChanged bool   // an existing application's status changed
	OldStatus     string // status before the change (meaningful if StatusChanged)
	ApplicationID int64
}

// statusRank orders statuses so that a late-arriving lower-rank mail (e.g. a
// duplicate "applied" confirmation) never downgrades the current status.
// rejected outranks everything: once rejected, a stray confirmation must not
// resurrect the application.
func statusRank(status string) int {
	switch status {
	case "applied":
		return 1
	case "interview":
		return 2
	case "offer":
		return 3
	case "rejected":
		return 4
	default:
		return 0
	}
}

// RecordEmail attaches a classified mail to the right application, creating
// one when needed. Matching order:
//  1. Same Gmail thread as an already-recorded mail.
//  2. Same normalized company and, when both sides know it, same position;
//     a mail without a position attaches to the company's most recent application.
//  3. Otherwise a new application is created.
//
// The application's current status only moves to the new mail's status when
// its rank is equal or higher (see statusRank); the mail itself is always
// recorded in application_emails.
func (s *Store) RecordEmail(company, position, via, status string, email Email) (RecordResult, error) {
	var res RecordResult

	companyNorm := Normalize(company)
	positionNorm := Normalize(position)
	now := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	appID, curStatus, err := s.matchApplication(tx, email.GmailThreadID, companyNorm, positionNorm)
	if err != nil {
		return res, err
	}

	if appID == 0 {
		// New application.
		r, err := tx.Exec(
			`INSERT INTO applications
				(company, company_norm, position, position_norm, via, status,
				 first_email_at, last_email_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			company, companyNorm, position, positionNorm, via, status,
			email.ReceivedAt, email.ReceivedAt, now, now,
		)
		if err != nil {
			return res, fmt.Errorf("insert application failed: %w", err)
		}
		appID, err = r.LastInsertId()
		if err != nil {
			return res, err
		}
		res.Created = true
	} else {
		// Existing application: move status only forward, never backward.
		if statusRank(status) >= statusRank(curStatus) {
			res.StatusChanged = status != curStatus
			res.OldStatus = curStatus
			_, err = tx.Exec(
				`UPDATE applications SET status = ?, last_email_at = ?, updated_at = ?,
				        via = COALESCE(NULLIF(via, ''), ?),
				        position = COALESCE(NULLIF(position, ''), ?),
				        position_norm = COALESCE(NULLIF(position_norm, ''), ?)
				  WHERE id = ?`,
				status, email.ReceivedAt, now, via, position, positionNorm, appID,
			)
		} else {
			// Lower-rank mail (e.g. duplicate confirmation): record only timestamps.
			_, err = tx.Exec(
				`UPDATE applications SET last_email_at = ?, updated_at = ? WHERE id = ?`,
				email.ReceivedAt, now, appID,
			)
		}
		if err != nil {
			return res, fmt.Errorf("update application failed: %w", err)
		}
	}

	_, err = tx.Exec(
		`INSERT INTO application_emails
			(application_id, gmail_message_id, gmail_thread_id, from_addr, subject,
			 status, raw_classification, received_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		appID, email.GmailMessageID, email.GmailThreadID, email.FromAddr, email.Subject,
		status, email.RawClassification, email.ReceivedAt,
	)
	if err != nil {
		return res, fmt.Errorf("insert email failed: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return res, err
	}
	res.ApplicationID = appID
	return res, nil
}

// matchApplication finds the application a mail belongs to (0 = none).
func (s *Store) matchApplication(tx *sql.Tx, threadID, companyNorm, positionNorm string) (int64, string, error) {
	var (
		id     int64
		status string
	)

	// 1) Thread match: another mail of the same Gmail thread.
	if threadID != "" {
		err := tx.QueryRow(
			`SELECT a.id, a.status FROM applications a
			  JOIN application_emails e ON e.application_id = a.id
			 WHERE e.gmail_thread_id = ? LIMIT 1`, threadID,
		).Scan(&id, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, "", err
		}
		if id != 0 {
			return id, status, nil
		}
	}

	if companyNorm == "" {
		return 0, "", nil
	}

	// 2a) Company + position match (when the mail names a position).
	if positionNorm != "" {
		err := tx.QueryRow(
			`SELECT id, status FROM applications
			  WHERE company_norm = ? AND position_norm = ?
			  ORDER BY id DESC LIMIT 1`, companyNorm, positionNorm,
		).Scan(&id, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, "", err
		}
		if id != 0 {
			return id, status, nil
		}
		// A positioned mail may also complete an application recorded without
		// a position (e.g. a LinkedIn confirmation that never named it).
		err = tx.QueryRow(
			`SELECT id, status FROM applications
			  WHERE company_norm = ? AND position_norm = ''
			  ORDER BY id DESC LIMIT 1`, companyNorm,
		).Scan(&id, &status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, "", err
		}
		return id, status, nil
	}

	// 2b) Mail has no position: attach to the company's most recent application.
	err := tx.QueryRow(
		`SELECT id, status FROM applications
		  WHERE company_norm = ? ORDER BY id DESC LIMIT 1`, companyNorm,
	).Scan(&id, &status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, "", err
	}
	return id, status, nil
}

// legalSuffixes are trailing tokens stripped by Normalize. Only unambiguous
// legal-entity suffixes are removed; words like "Games" stay because removing
// them could wrongly merge different companies.
var legalSuffixes = map[string]bool{
	"a.ş": true, "a.ş.": true, "a.s": true, "a.s.": true, "aş": true,
	"inc": true, "inc.": true, "llc": true, "llc.": true,
	"ltd": true, "ltd.": true, "ltd.şti": true, "ltd.şti.": true, "şti": true, "şti.": true,
	"gmbh": true, "co": true, "co.": true, "corp": true, "corp.": true,
	"b.v": true, "b.v.": true, "bv": true, "s.a": true, "s.a.": true,
}

// Normalize lowercases, trims and strips trailing legal suffixes from a
// company/position name so near-identical LLM outputs merge.
func Normalize(s string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(s)))
	for len(fields) > 0 {
		last := strings.Trim(fields[len(fields)-1], ",;")
		if legalSuffixes[last] {
			fields = fields[:len(fields)-1]
			continue
		}
		break
	}
	return strings.Join(fields, " ")
}

// ListWithEmails returns all applications ordered by last mail, descending,
// each with its full email history (newest first) for the chained UI view.
func (s *Store) ListWithEmails() ([]Application, error) {
	rows, err := s.db.Query(
		`SELECT id, company, position, via, status, first_email_at, last_email_at
		   FROM applications
		  ORDER BY last_email_at DESC, id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []Application
	index := map[int64]int{}
	for rows.Next() {
		var a Application
		var first, last sql.NullTime
		if err := rows.Scan(&a.ID, &a.Company, &a.Position, &a.Via, &a.Status, &first, &last); err != nil {
			return nil, err
		}
		if first.Valid {
			a.FirstEmailAt = first.Time
		}
		if last.Valid {
			a.LastEmailAt = last.Time
		}
		index[a.ID] = len(apps)
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	erows, err := s.db.Query(
		`SELECT id, application_id, gmail_message_id, gmail_thread_id, from_addr,
		        subject, status, raw_classification, received_at
		   FROM application_emails
		  ORDER BY received_at DESC, id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer erows.Close()

	for erows.Next() {
		var e Email
		var received sql.NullTime
		if err := erows.Scan(&e.ID, &e.ApplicationID, &e.GmailMessageID, &e.GmailThreadID,
			&e.FromAddr, &e.Subject, &e.Status, &e.RawClassification, &received); err != nil {
			return nil, err
		}
		if received.Valid {
			e.ReceivedAt = received.Time
		}
		if i, ok := index[e.ApplicationID]; ok {
			apps[i].Emails = append(apps[i].Emails, e)
		}
	}
	return apps, erows.Err()
}
