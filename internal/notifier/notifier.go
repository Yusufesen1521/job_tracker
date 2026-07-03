// Package notifier sends notifications on status changes.
// Notifier is an interface; the default implementation uses ntfy.sh.
package notifier

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Notifier is the notification-sending abstraction.
type Notifier interface {
	Notify(ctx context.Context, title, message string) error
}

// Ntfy is the ntfy.sh implementation of Notifier.
// If the topic is empty it sends nothing (no-op) — convenient for local dev.
type Ntfy struct {
	topic string
	http  *http.Client
}

// NewNtfy creates an Ntfy notifier for the given topic URL.
// topic example: https://ntfy.sh/my-secret-topic
func NewNtfy(topic string) *Ntfy {
	return &Ntfy{
		topic: strings.TrimSpace(topic),
		http:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Notify sends a simple HTTP POST to the ntfy topic.
func (n *Ntfy) Notify(ctx context.Context, title, message string) error {
	if n.topic == "" {
		return nil // notifications not configured
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.topic, strings.NewReader(message))
	if err != nil {
		return err
	}
	// ntfy reads the title and priority from headers.
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "briefcase")

	resp, err := n.http.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy HTTP %d", resp.StatusCode)
	}
	return nil
}
