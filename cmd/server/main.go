// Command server renders the applications in a single-page HTML table.
// net/http + html/template; no framework.
package main

import (
	"crypto/subtle"
	"embed"
	"html/template"
	"log"
	"net/http"
	"time"

	"github.com/Yusufesen1521/job_tracker/internal/config"
	"github.com/Yusufesen1521/job_tracker/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("could not load config: %v", err)
	}

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("could not open store: %v", err)
	}
	defer db.Close()

	tmpl := template.Must(template.New("index.html").Funcs(template.FuncMap{
		"fmtTime": func(t time.Time) string {
			if t.IsZero() {
				return "-"
			}
			return t.Local().Format("2006-01-02 15:04")
		},
	}).ParseFS(templatesFS, "templates/index.html"))

	h := &handler{db: db, tmpl: tmpl}

	mux := http.NewServeMux()
	mux.HandleFunc("/", h.index)

	// Basic Auth: enabled when both WEB_USER and WEB_PASS are set.
	wrapped := basicAuth(mux, cfg.WebUser, cfg.WebPass)

	srv := &http.Server{
		Addr:              cfg.WebAddr,
		Handler:           wrapped,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("server listening: %s (auth=%v)", cfg.WebAddr, cfg.WebUser != "" && cfg.WebPass != "")
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

type handler struct {
	db   *store.Store
	tmpl *template.Template
}

func (h *handler) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	apps, err := h.db.ListWithEmails()
	if err != nil {
		http.Error(w, "database error", http.StatusInternalServerError)
		log.Printf("list error: %v", err)
		return
	}
	data := struct {
		Apps      []store.Application
		Total     int
		Generated time.Time
	}{Apps: apps, Total: len(apps), Generated: time.Now()}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.tmpl.Execute(w, data); err != nil {
		log.Printf("template error: %v", err)
	}
}

// basicAuth enforces HTTP Basic Auth when user/pass are set; passive otherwise.
// Comparison is constant-time (subtle).
func basicAuth(next http.Handler, user, pass string) http.Handler {
	if user == "" || pass == "" {
		return next // auth disabled
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="job_tracker"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
