package gmail

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// AuthorizeInteractive runs the one-time OAuth flow for initial setup.
// It starts a temporary local HTTP server (loopback), prints the URL to open
// in a browser, captures the authorization code from Google's redirect
// automatically, and saves the token to tokenFile (e.g. token.json).
//
// Google removed the "paste the code manually" (OOB) flow, so the loopback
// redirect is the standard method for Desktop clients. The resulting
// token.json can later be copied to a VPS for headless operation.
func AuthorizeInteractive(ctx context.Context, credentialsFile, tokenFile string) error {
	cfg, err := oauthConfig(credentialsFile)
	if err != nil {
		return err
	}

	// Loopback server: listen on an OS-assigned free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("could not start local callback server: %w", err)
	}
	defer ln.Close()

	cfg.RedirectURL = fmt.Sprintf("http://%s/", ln.Addr().String())

	// Random state for CSRF protection.
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		return err
	}
	state := hex.EncodeToString(stateBytes)

	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	fmt.Println("Open the following URL in your browser and authorize your Gmail account:")
	fmt.Println()
	fmt.Println("   " + authURL)
	fmt.Println()
	fmt.Println("The code will be captured automatically once you authorize. Waiting...")

	type callbackResult struct {
		code string
		err  error
	}
	resultCh := make(chan callbackResult, 1)

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			resultCh <- callbackResult{err: fmt.Errorf("OAuth state mismatch (CSRF?)")}
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, "authorization denied: "+e, http.StatusBadRequest)
			resultCh <- callbackResult{err: fmt.Errorf("authorization denied: %s", e)}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			resultCh <- callbackResult{err: fmt.Errorf("callback has no authorization code")}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<h2>Authorization successful ✔</h2><p>You can close this tab and return to the terminal.</p>")
		resultCh <- callbackResult{code: code}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	// Wait for the callback (10-minute timeout).
	var code string
	select {
	case r := <-resultCh:
		if r.err != nil {
			return r.err
		}
		code = r.code
	case <-time.After(10 * time.Minute):
		return fmt.Errorf("OAuth callback timed out (10m)")
	case <-ctx.Done():
		return ctx.Err()
	}

	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("could not exchange code for token: %w", err)
	}

	if err := saveToken(tokenFile, tok); err != nil {
		return fmt.Errorf("could not save token: %w", err)
	}

	fmt.Printf("\nSuccess! Token saved: %s\n", tokenFile)
	fmt.Println("Copy this file to your VPS for headless operation. NEVER commit it.")
	return nil
}
