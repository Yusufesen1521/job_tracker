// Package gmail fetches mail via the official Gmail API + OAuth2 (not IMAP).
// Secrets (credentials.json, token.json) are read from files and never live
// in the repository.
package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
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
		full, err := c.svc.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
		if err != nil {
			return nil, fmt.Errorf("could not fetch message %s: %w", id, err)
		}
		out = append(out, toMessage(full))
	}
	return out, nil
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
