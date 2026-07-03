// Command poller fetches Gmail, classifies mails, writes them to the DB and
// sends a notification when a status changes.
//
// Usage:
//
//	poller --auth   # first-time setup: runs OAuth and produces token.json
//	poller --check  # tests whether the LLM API works / quota status
//	poller --once   # fetch-process once and exit (ideal for cron)
//	poller          # runs continuously at POLL_INTERVAL
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Yusufesen1521/job_tracker/internal/classifier"
	"github.com/Yusufesen1521/job_tracker/internal/config"
	"github.com/Yusufesen1521/job_tracker/internal/gmail"
	"github.com/Yusufesen1521/job_tracker/internal/notifier"
	"github.com/Yusufesen1521/job_tracker/internal/store"
)

func main() {
	var (
		authFlag  = flag.Bool("auth", false, "run the OAuth flow and produce token.json")
		onceFlag  = flag.Bool("once", false, "fetch-process once and exit (for cron)")
		checkFlag = flag.Bool("check", false, "run an LLM API health/quota check and exit")
	)
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("could not load config: %v", err)
	}

	ctx := context.Background()

	// First-time setup: OAuth flow.
	if *authFlag {
		if err := gmail.AuthorizeInteractive(ctx, cfg.GmailCredentialsFile, cfg.GmailTokenFile); err != nil {
			log.Fatalf("OAuth failed: %v", err)
		}
		return
	}

	if err := cfg.RequirePoller(); err != nil {
		log.Fatalf("%v", err)
	}

	clf, provider := newClassifier(cfg)

	// API health/quota check: tests only the LLM, does not touch Gmail.
	if *checkFlag {
		if err := checkAPI(ctx, clf, provider); err != nil {
			log.Fatalf("%v", err)
		}
		return
	}

	// Wire up dependencies.
	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("could not open store: %v", err)
	}
	defer db.Close()

	gm, err := gmail.NewClient(ctx, cfg.GmailCredentialsFile, cfg.GmailTokenFile)
	if err != nil {
		log.Fatalf("gmail client: %v", err)
	}

	filter := gmail.NewFilter(cfg.FilterKeywords, cfg.FilterExcludeDomains)
	ntfy := notifier.NewNtfy(cfg.NtfyTopic)

	// Hybrid pre-screening: ask a cheap LLM when no keyword matches.
	var screener *classifier.Screener
	if cfg.PrefilterMode == "hybrid" {
		if cfg.OpenRouterAPIKey == "" {
			log.Fatalf("PREFILTER_MODE=hybrid requires OPENROUTER_API_KEY")
		}
		screener = classifier.NewScreener(cfg.OpenRouterAPIKey, cfg.PrefilterModel)
		log.Printf("hybrid pre-screening enabled: %s", cfg.PrefilterModel)
	}

	w := &worker{
		cfg:      cfg,
		db:       db,
		gm:       gm,
		filter:   filter,
		screener: screener,
		clf:      clf,
		ntfy:     ntfy,
	}

	if *onceFlag {
		if err := w.runOnce(ctx); err != nil {
			log.Fatalf("run error: %v", err)
		}
		return
	}

	// Ticker mode: run once immediately, then continue at POLL_INTERVAL.
	log.Printf("poller started, interval: %s", cfg.PollInterval)
	if err := w.runOnce(ctx); err != nil {
		log.Printf("initial run error: %v", err)
	}

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	// Catch signals for a clean shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			if err := w.runOnce(ctx); err != nil {
				log.Printf("run error: %v", err)
			}
		case <-sig:
			log.Println("shutdown signal received, exiting")
			return
		}
	}
}

// newClassifier picks the Classifier implementation based on LLM_PROVIDER.
func newClassifier(cfg *config.Config) (classifier.Classifier, string) {
	switch cfg.LLMProvider {
	case "openrouter":
		return classifier.NewOpenRouter(cfg.OpenRouterAPIKey, cfg.OpenRouterModel),
			"openrouter/" + cfg.OpenRouterModel
	case "anthropic":
		return classifier.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicModel),
			"anthropic/" + cfg.AnthropicModel
	default: // gemini
		return classifier.NewGemini(cfg.GeminiAPIKey, cfg.GeminiModel),
			"gemini/" + cfg.GeminiModel
	}
}

// checkAPI sends a sample mail to the LLM and reports health and quota status.
// When the quota is exhausted (HTTP 429) it fails with a clear message; run it
// from cron or manually to notice exhausted limits early and switch providers.
func checkAPI(ctx context.Context, clf classifier.Classifier, provider string) error {
	fmt.Printf("API check: %s\n", provider)

	res, err := clf.Classify(ctx, classifier.Email{
		From:    "careers@example-corp.com",
		Subject: "Your application to Example Corp",
		Body:    "Thank you for applying to the Software Engineer position at Example Corp. We have received your application.",
	})
	if err != nil {
		if errors.Is(err, classifier.ErrQuotaExhausted) {
			return fmt.Errorf("QUOTA EXHAUSTED — try another provider (LLM_PROVIDER=openrouter): %w", err)
		}
		return fmt.Errorf("API ERROR: %w", err)
	}

	fmt.Printf("OK — sample classification: job_related=%v company=%q status=%q confidence=%.2f\n",
		res.IsJobRelated, res.Company, res.Status, res.Confidence)
	if !res.IsJobRelated || res.Status != "applied" {
		fmt.Println("WARNING: the sample mail should have been classified as 'applied'; model behavior differs from expectations.")
	}
	return nil
}

// worker carries the dependencies of a single poll cycle.
type worker struct {
	cfg      *config.Config
	db       *store.Store
	gm       *gmail.Client
	filter   *gmail.Filter
	screener *classifier.Screener // nil disables hybrid pre-screening
	clf      classifier.Classifier
	ntfy     notifier.Notifier
}

// runOnce executes a single fetch-classify-store cycle.
func (w *worker) runOnce(ctx context.Context) error {
	msgs, err := w.gm.List(ctx, w.cfg.GmailQuery, w.cfg.GmailMaxResults)
	if err != nil {
		return err
	}
	log.Printf("fetched %d mails", len(msgs))

	// If the screener quota runs out mid-cycle we stop asking for the rest;
	// keyword-missed mails count as rejected this cycle (they are re-seen next cycle).
	screenerActive := w.screener != nil

	for _, m := range msgs {
		// 1) Dedup: skip mails processed before.
		exists, err := w.db.MessageExists(m.ID)
		if err != nil {
			log.Printf("dedup check error (%s): %v", m.ID, err)
			continue
		}
		if exists {
			continue
		}

		// 2) Cheap pre-filter (+ the small-LLM second opinion in hybrid mode).
		switch verdict, reason := w.filter.Verdict(m); verdict {
		case gmail.ExcludedDomain:
			log.Printf("filter rejected (%s): %s", m.Subject, reason)
			continue
		case gmail.NoKeyword:
			if !screenerActive {
				log.Printf("filter rejected (%s): %s", m.Subject, reason)
				continue
			}
			ok, err := w.screener.IsJobRelated(ctx, classifier.Email{
				From: m.From, Subject: m.Subject, Body: bodyOrSnippet(m),
			})
			if err != nil {
				if errors.Is(err, classifier.ErrQuotaExhausted) {
					log.Printf("screener quota exhausted, disabled for this cycle: %v", err)
					screenerActive = false
				} else {
					log.Printf("screener error (%s): %v", m.Subject, err)
				}
				continue
			}
			if !ok {
				log.Printf("screener rejected (%s)", m.Subject)
				continue
			}
			log.Printf("screener passed (%s) — no keyword had matched", m.Subject)
		}

		// 3) LLM classification.
		res, err := w.clf.Classify(ctx, classifier.Email{
			From:    m.From,
			Subject: m.Subject,
			Body:    bodyOrSnippet(m),
		})
		if err != nil {
			// If the quota is exhausted, trying the remaining mails is pointless.
			if errors.Is(err, classifier.ErrQuotaExhausted) {
				return fmt.Errorf("LLM quota exhausted, cycle stopped (remaining mails next cycle): %w", err)
			}
			log.Printf("classification error (%s): %v", m.Subject, err)
			continue
		}

		// 4) Unrelated or low confidence → don't store.
		if !res.IsJobRelated || res.Confidence < w.cfg.ConfidenceThreshold {
			log.Printf("skipped (job_related=%v, conf=%.2f): %s", res.IsJobRelated, res.Confidence, m.Subject)
			continue
		}

		// 5) Write to the DB (thread semantics).
		raw, _ := json.Marshal(res)
		now := time.Now().UTC()
		applied := m.AppliedAt
		if applied.IsZero() {
			applied = now
		}
		up, err := w.db.UpsertByThread(store.Application{
			Company:           res.Company,
			Status:            res.Status,
			EmailMessageID:    m.ID,
			EmailThreadID:     m.ThreadID,
			Subject:           m.Subject,
			AppliedAt:         applied,
			UpdatedAt:         now,
			RawClassification: string(raw),
		})
		if err != nil {
			log.Printf("DB write error (%s): %v", m.Subject, err)
			continue
		}

		// 6) Notify: new application or status change.
		switch {
		case up.Created:
			log.Printf("new application: %s [%s]", res.Company, res.Status)
			w.notify(ctx, "New application: "+res.Company,
				res.Company+" — status: "+res.Status)
		case up.StatusChanged:
			log.Printf("status changed: %s [%s → %s]", res.Company, up.OldStatus, res.Status)
			w.notify(ctx, "Status updated: "+res.Company,
				res.Company+": "+up.OldStatus+" → "+res.Status)
		}
	}
	return nil
}

func (w *worker) notify(ctx context.Context, title, msg string) {
	if err := w.ntfy.Notify(ctx, title, msg); err != nil {
		log.Printf("notification error: %v", err)
	}
}

// bodyOrSnippet falls back to the snippet when the body is empty.
func bodyOrSnippet(m gmail.Message) string {
	if m.Body != "" {
		return m.Body
	}
	return m.Snippet
}
