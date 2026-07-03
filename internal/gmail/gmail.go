// Package gmail fetches mail via the official Gmail API + OAuth2 (not IMAP).
// Secrets (credentials.json, token.json) are read from files and never live
// in the repository.
package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// Message is the simplified form of a fetched mail.
type Message struct {
	ID        string
	ThreadID  string
	From      string
	Subject   string
	Snippet   string
	Body      string    // plain-text body (if available)
	AppliedAt time.Time // when the mail arrived in Gmail (internalDate)
}

// Client wraps the Gmail API.
type Client struct {
	svc *gmail.Service
}

// NewClient builds an authorized client from credentials.json and token.json.
// If token.json is missing it returns an error; run AuthorizeInteractive first.
func NewClient(ctx context.Context, credentialsFile, tokenFile string) (*Client, error) {
	cfg, err := oauthConfig(credentialsFile)
	if err != nil {
		return nil, err
	}

	tok, err := loadToken(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("could not read token file (%s) — run the OAuth flow first: %w", tokenFile, err)
	}

	// The TokenSource auto-refreshes the access token using the refresh token
	// (critical for headless/VPS operation).
	httpClient := cfg.Client(ctx, tok)
	svc, err := gmail.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("could not create gmail service: %w", err)
	}
	return &Client{svc: svc}, nil
}

// oauthConfig builds an OAuth2 config from credentials.json (read-only scope).
func oauthConfig(credentialsFile string) (*oauth2.Config, error) {
	b, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("could not read credentials file (%s): %w", credentialsFile, err)
	}
	cfg, err := google.ConfigFromJSON(b, gmail.GmailReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("could not parse credentials file: %w", err)
	}
	return cfg, nil
}

// List fetches mails matching the query and converts them to Messages.
// The Gmail API returns at most 500 results per page; pagination continues
// until maxResults is reached (or results run out).
func (c *Client) List(ctx context.Context, query string, maxResults int64) ([]Message, error) {
	var ids []string
	pageToken := ""
	for int64(len(ids)) < maxResults {
		pageSize := maxResults - int64(len(ids))
		if pageSize > 500 {
			pageSize = 500
		}
		call := c.svc.Users.Messages.List("me").Q(query).MaxResults(pageSize)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("could not list messages: %w", err)
		}
		for _, m := range resp.Messages {
			ids = append(ids, m.Id)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}

	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		m, err := c.GetMessage(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// GetMessage fetches a single mail by ID and converts it to a Message.
func (c *Client) GetMessage(ctx context.Context, id string) (Message, error) {
	full, err := c.svc.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
	if err != nil {
		return Message{}, fmt.Errorf("could not fetch message %s: %w", id, err)
	}
	return toMessage(full), nil
}

// ProfileHistoryID returns the mailbox's current historyId, used as the
// starting point for incremental ListHistorySince polling.
func (c *Client) ProfileHistoryID(ctx context.Context) (uint64, error) {
	p, err := c.svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return 0, fmt.Errorf("could not fetch profile: %w", err)
	}
	return p.HistoryId, nil
}

// ListHistorySince returns the IDs of mails added since the given historyId.
// This call is nearly free quota-wise and returns empty when nothing changed,
// which makes short-interval near-real-time polling viable.
//
// expired=true means Gmail no longer remembers that historyId (it expires
// after roughly a week); the caller should fall back to a full query scan
// and fetch a fresh historyId via ProfileHistoryID.
func (c *Client) ListHistorySince(ctx context.Context, historyID uint64) (msgIDs []string, newHistoryID uint64, expired bool, err error) {
	newHistoryID = historyID
	pageToken := ""
	for {
		call := c.svc.Users.History.List("me").
			StartHistoryId(historyID).
			HistoryTypes("messageAdded")
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		resp, err := call.Context(ctx).Do()
		if err != nil {
			var gerr *googleapi.Error
			if errors.As(err, &gerr) && gerr.Code == http.StatusNotFound {
				// historyId expired: caller must resync with a full scan.
				return nil, historyID, true, nil
			}
			return nil, historyID, false, fmt.Errorf("could not list history: %w", err)
		}
		if resp.HistoryId > newHistoryID {
			newHistoryID = resp.HistoryId
		}
		for _, h := range resp.History {
			for _, ma := range h.MessagesAdded {
				if ma.Message != nil {
					msgIDs = append(msgIDs, ma.Message.Id)
				}
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return msgIDs, newHistoryID, false, nil
}

// toMessage converts a Gmail API message into the simplified Message.
func toMessage(m *gmail.Message) Message {
	msg := Message{
		ID:       m.Id,
		ThreadID: m.ThreadId,
		Snippet:  m.Snippet,
	}
	if m.InternalDate > 0 {
		msg.AppliedAt = time.UnixMilli(m.InternalDate)
	}
	if m.Payload != nil {
		for _, h := range m.Payload.Headers {
			switch strings.ToLower(h.Name) {
			case "from":
				msg.From = h.Value
			case "subject":
				msg.Subject = h.Value
			}
		}
		msg.Body = extractBody(m.Payload)
	}
	return msg
}

// extractBody pulls plain text (text/plain preferred) from the mail body.
func extractBody(part *gmail.MessagePart) string {
	if part == nil {
		return ""
	}
	// Prefer text/plain.
	if part.MimeType == "text/plain" && part.Body != nil && part.Body.Data != "" {
		return decodeBody(part.Body.Data)
	}
	// Walk sub-parts of multipart mails.
	var plain, html string
	for _, p := range part.Parts {
		switch {
		case p.MimeType == "text/plain" && p.Body != nil && p.Body.Data != "":
			plain += decodeBody(p.Body.Data)
		case p.MimeType == "text/html" && p.Body != nil && p.Body.Data != "":
			html += decodeBody(p.Body.Data)
		case strings.HasPrefix(p.MimeType, "multipart/"):
			if s := extractBody(p); s != "" {
				plain += s
			}
		}
	}
	if plain != "" {
		return plain
	}
	return html
}

// decodeBody decodes Gmail's URL-safe base64 body.
func decodeBody(data string) string {
	b, err := base64.URLEncoding.DecodeString(data)
	if err != nil {
		// Some bodies come without padding; try RawURLEncoding.
		b, err = base64.RawURLEncoding.DecodeString(data)
		if err != nil {
			return ""
		}
	}
	return string(b)
}

// --- OAuth token management ---

func loadToken(path string) (*oauth2.Token, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(b, &tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	b, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	// 0600: owner-only read (this is a secret).
	return os.WriteFile(path, b, 0o600)
}
