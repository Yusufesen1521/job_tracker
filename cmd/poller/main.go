// Command poller fetches Gmail, classifies mails, writes them to the DB and
// sends a notification when a status changes.
//
// Usage:
//
//	poller --auth   # first-time setup: runs OAuth and produces token.json
//	poller --check  # tests whether the LLM API works / quota status
//	poller --once   # full-scan-process once and exit (ideal for cron/backfill)
//	poller          # real-time mode: full scan once, then near-real-time
//	                # Gmail history polling every POLL_INTERVAL (default 45s)
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
	"strconv"
	"syscall"
	"time"

	"github.com/Yusufesen1521/job_tracker/internal/classifier"
	"github.com/Yusufesen1521/job_tracker/internal/config"
	"github.com/Yusufesen1521/job_tracker/internal/gmail"
	"github.com/Yusufesen1521/job_tracker/internal/notifier"
	"github.com/Yusufesen1521/job_tracker/internal/store"
)

// metaHistoryKey is the meta table key holding the last processed Gmail historyId.
const metaHistoryKey = "last_history_id"

func main() {
	var (
		authFlag  = flag.Bool("auth", false, "run the OAuth flow and produce token.json")
		onceFlag  = flag.Bool("once", false, "full-scan-process once and exit (for cron)")
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
		ntfy:     newNotifier(cfg),
	}

	if *onceFlag {
		if err := w.fullScan(ctx); err != nil {
			log.Fatalf("run error: %v", err)
		}
		return
	}

	w.runRealtime(ctx)
}

// runRealtime does one full scan, then watches the mailbox via the Gmail
// history API: a nearly-free "anything new?" call every POLL_INTERVAL that
// returns only new message IDs, so only genuinely new mails are processed.
func (w *worker) runRealtime(ctx context.Context) {
	log.Printf("real-time mode: initial full scan, then history polling every %s", w.cfg.PollInterval)

	if err := w.fullScan(ctx); err != nil {
		log.Printf("initial scan error: %v", err)
	}
	if err := w.resyncHistoryID(ctx); err != nil {
		log.Fatalf("could not fetch initial historyId: %v", err)
	}

	ticker := time.NewTicker(w.cfg.PollInterval)
	defer ticker.Stop()

	// Catch signals for a clean shutdown.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	for {
		select {
		case <-ticker.C:
			if err := w.pollHistory(ctx); err != nil {
				log.Printf("history poll error: %v", err)
			}
		case <-sig:
			log.Println("shutdown signal received, exiting")
			return
		}
	}
}

// pollHistory processes mails that arrived since the stored historyId.
func (w *worker) pollHistory(ctx context.Context) error {
	stored, err := w.db.GetMeta(metaHistoryKey)
	if err != nil {
		return err
	}
	historyID, _ := strconv.ParseUint(stored, 10, 64)
	if historyID == 0 {
		return w.resyncHistoryID(ctx)
	}

	msgIDs, newHistoryID, expired, err := w.gm.ListHistorySince(ctx, historyID)
	if err != nil {
		return err
	}
	if expired {
		// historyId older than Gmail remembers (~a week offline): self-heal
		// with a full scan and a fresh historyId.
		log.Printf("historyId expired, resyncing with a full scan")
		if err := w.fullScan(ctx); err != nil {
			return err
		}
		return w.resyncHistoryID(ctx)
	}

	for _, id := range msgIDs {
		m, err := w.gm.GetMessage(ctx, id)
		if err != nil {
			log.Printf("could not fetch new mail %s: %v", id, err)
			continue
		}
		log.Printf("new mail: %s", m.Subject)
		if err := w.processMessage(ctx, m, &screenerState{active: w.screener != nil}); err != nil {
			// Quota exhausted: keep the old historyId so these mails are
			// retried on a later poll, and surface the error.
			return err
		}
	}

	return w.db.SetMeta(metaHistoryKey, strconv.FormatUint(newHistoryID, 10))
}

// resyncHistoryID stores the mailbox's current historyId as the new baseline.
func (w *worker) resyncHistoryID(ctx context.Context) error {
	id, err := w.gm.ProfileHistoryID(ctx)
	if err != nil {
		return err
	}
	return w.db.SetMeta(metaHistoryKey, strconv.FormatUint(id, 10))
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

// newNotifier picks the Notifier implementation based on NOTIFIER.
func newNotifier(cfg *config.Config) notifier.Notifier {
	switch cfg.Notifier {
	case "telegram":
		log.Printf("notifications: telegram")
		return notifier.NewTelegram(cfg.TelegramBotToken, cfg.TelegramChatID)
	case "ntfy":
		log.Printf("notifications: ntfy")
		return notifier.NewNtfy(cfg.NtfyTopic)
	default:
		return notifier.NewNtfy("") // no-op
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

	fmt.Printf("OK — sample classification: job_related=%v company=%q position=%q status=%q confidence=%.2f\n",
		res.IsJobRelated, res.Company, res.Position, res.Status, res.Confidence)
	if !res.IsJobRelated || res.Status != "applied" {
		fmt.Println("WARNING: the sample mail should have been classified as 'applied'; model behavior differs from expectations.")
	}
	return nil
}

// worker carries the dependencies of the poller.
type worker struct {
	cfg      *config.Config
	db       *store.Store
	gm       *gmail.Client
	filter   *gmail.Filter
	screener *classifier.Screener // nil disables hybrid pre-screening
	clf      classifier.Classifier
	ntfy     notifier.Notifier
}

// screenerState tracks whether the screener is still usable within one scan;
// it disables itself for the rest of the scan when its quota runs out.
type screenerState struct {
	active bool
}

// fullScan runs a fetch-classify-store cycle over the whole GMAIL_QUERY window.
func (w *worker) fullScan(ctx context.Context) error {
	msgs, err := w.gm.List(ctx, w.cfg.GmailQuery, w.cfg.GmailMaxResults)
	if err != nil {
		return err
	}
	log.Printf("fetched %d mails", len(msgs))

	st := &screenerState{active: w.screener != nil}
	for _, m := range msgs {
		if err := w.processMessage(ctx, m, st); err != nil {
			return err
		}
	}
	return nil
}

// processMessage runs one mail through the pipeline:
// dedup → pre-filter (+screener) → classify → record → notify.
// A non-nil error means the whole scan should stop (quota exhausted);
// per-mail problems are logged and swallowed.
func (w *worker) processMessage(ctx context.Context, m gmail.Message, st *screenerState) error {
	// 1) Dedup: skip mails processed before.
	exists, err := w.db.MessageExists(m.ID)
	if err != nil {
		log.Printf("dedup check error (%s): %v", m.ID, err)
		return nil
	}
	if exists {
		return nil
	}

	// 2) Cheap pre-filter (+ the small-LLM second opinion in hybrid mode).
	switch verdict, reason := w.filter.Verdict(m); verdict {
	case gmail.ExcludedDomain:
		log.Printf("filter rejected (%s): %s", m.Subject, reason)
		return nil
	case gmail.NoKeyword:
		if !st.active {
			log.Printf("filter rejected (%s): %s", m.Subject, reason)
			return nil
		}
		ok, err := w.screener.IsJobRelated(ctx, classifier.Email{
			From: m.From, Subject: m.Subject, Body: bodyOrSnippet(m),
		})
		if err != nil {
			if errors.Is(err, classifier.ErrQuotaExhausted) {
				log.Printf("screener quota exhausted, disabled for this scan: %v", err)
				st.active = false
			} else {
				log.Printf("screener error (%s): %v", m.Subject, err)
			}
			return nil
		}
		if !ok {
			log.Printf("screener rejected (%s)", m.Subject)
			return nil
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
		// If the quota is exhausted, continuing is pointless.
		if errors.Is(err, classifier.ErrQuotaExhausted) {
			return fmt.Errorf("LLM quota exhausted, scan stopped (remaining mails retried later): %w", err)
		}
		log.Printf("classification error (%s): %v", m.Subject, err)
		return nil
	}

	// 4) Unrelated or low confidence → don't store.
	if !res.IsJobRelated || res.Confidence < w.cfg.ConfidenceThreshold {
		log.Printf("skipped (job_related=%v, conf=%.2f): %s", res.IsJobRelated, res.Confidence, m.Subject)
		return nil
	}

	// 5) Record: attach the mail to the right application (thread → company
	// [+position] matching; status never moves backward).
	raw, _ := json.Marshal(res)
	received := m.AppliedAt
	if received.IsZero() {
		received = time.Now().UTC()
	}
	rec, err := w.db.RecordEmail(res.Company, res.Position, res.Via, res.Status, store.Email{
		GmailMessageID:    m.ID,
		GmailThreadID:     m.ThreadID,
		FromAddr:          m.From,
		Subject:           m.Subject,
		RawClassification: string(raw),
		ReceivedAt:        received,
	})
	if err != nil {
		log.Printf("DB write error (%s): %v", m.Subject, err)
		return nil
	}

	// 6) Notify: new application or status change.
	label := res.Company
	if res.Position != "" {
		label += " (" + res.Position + ")"
	}
	switch {
	case rec.Created:
		log.Printf("new application: %s [%s]", label, res.Status)
		w.notify(ctx, "New application: "+res.Company, label+" — status: "+res.Status)
	case rec.StatusChanged:
		log.Printf("status changed: %s [%s → %s]", label, rec.OldStatus, res.Status)
		w.notify(ctx, "Status updated: "+res.Company, label+": "+rec.OldStatus+" → "+res.Status)
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
