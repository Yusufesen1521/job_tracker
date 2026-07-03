package notifier

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Telegram is the Telegram Bot API implementation of Notifier.
// Setup: create a bot via @BotFather (token), message the bot once, then read
// your chat ID from https://api.telegram.org/bot<TOKEN>/getUpdates.
type Telegram struct {
	token    string
	chatID   string
	http     *http.Client
	endpoint string // overridable in tests; empty means the real API
}

// NewTelegram creates a Telegram notifier.
func NewTelegram(token, chatID string) *Telegram {
	return &Telegram{
		token:  strings.TrimSpace(token),
		chatID: strings.TrimSpace(chatID),
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

// Notify sends the message via the Bot API's sendMessage method.
func (t *Telegram) Notify(ctx context.Context, title, message string) error {
	if t.token == "" || t.chatID == "" {
		return nil // notifications not configured
	}

	endpoint := t.endpoint
	if endpoint == "" {
		endpoint = "https://api.telegram.org/bot" + t.token + "/sendMessage"
	}

	form := url.Values{
		"chat_id": {t.chatID},
		"text":    {title + "\n" + message},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := t.http.Do(req)
	if err != nil {
		return fmt.Errorf("telegram request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram HTTP %d", resp.StatusCode)
	}
	return nil
}
