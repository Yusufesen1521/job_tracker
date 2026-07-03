// Package config reads all settings and secrets from environment variables.
// If a .env file exists (local development), it is loaded via godotenv.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all runtime settings for the application.
type Config struct {
	// LLM provider selection: "gemini" | "openrouter" | "anthropic"
	LLMProvider string

	// Anthropic
	AnthropicAPIKey string
	AnthropicModel  string

	// Gemini
	GeminiAPIKey string
	GeminiModel  string

	// OpenRouter
	OpenRouterAPIKey string
	OpenRouterModel  string

	// Gmail
	GmailCredentialsFile string
	GmailTokenFile       string
	GmailQuery           string
	GmailMaxResults      int64

	// Store
	DBPath string

	// Notifier: "telegram" | "ntfy" | "none".
	// Default: telegram when its token is set, else ntfy when its topic is set.
	Notifier         string
	NtfyTopic        string
	TelegramBotToken string
	TelegramChatID   string

	// Poller
	PollInterval        time.Duration
	ConfidenceThreshold float64

	// Pre-filter
	FilterKeywords       []string
	FilterExcludeDomains []string
	// PrefilterMode: "keywords" (string checks only) | "hybrid"
	// (ask a cheap LLM when no keyword matches). Hybrid requires an OpenRouter key.
	PrefilterMode  string
	PrefilterModel string

	// Web
	WebAddr string
	WebUser string
	WebPass string
}

// Load reads .env (if present) and populates the Config.
func Load() (*Config, error) {
	// .env is optional: absence is not an error, plain OS env is used instead.
	_ = godotenv.Load()

	cfg := &Config{
		LLMProvider:          strings.ToLower(getEnv("LLM_PROVIDER", "gemini")),
		AnthropicAPIKey:      os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:       getEnv("ANTHROPIC_MODEL", "claude-haiku-4-5-20251001"),
		GeminiAPIKey:         getEnv("GEMINI_API_KEY", os.Getenv("GeminiApi")),
		GeminiModel:          getEnv("GEMINI_MODEL", "gemini-3.1-flash-lite"),
		OpenRouterAPIKey:     getEnv("OPENROUTER_API_KEY", os.Getenv("OpenRouterApi")),
		OpenRouterModel:      getEnv("OPENROUTER_MODEL", "meta-llama/llama-3.3-70b-instruct:free"),
		GmailCredentialsFile: getEnv("GMAIL_CREDENTIALS_FILE", "credentials.json"),
		GmailTokenFile:       getEnv("GMAIL_TOKEN_FILE", "token.json"),
		GmailQuery:           getEnv("GMAIL_QUERY", "newer_than:7d"),
		GmailMaxResults:      getEnvInt64("GMAIL_MAX_RESULTS", 50),
		DBPath:               getEnv("DB_PATH", "job_tracker.db"),
		NtfyTopic:            os.Getenv("NTFY_TOPIC"),
		TelegramBotToken:     os.Getenv("TELEGRAM_BOT_TOKEN"),
		TelegramChatID:       os.Getenv("TELEGRAM_CHAT_ID"),
		ConfidenceThreshold:  getEnvFloat("CONFIDENCE_THRESHOLD", 0.6),
		FilterKeywords:       splitCSV(getEnv("FILTER_KEYWORDS", "")),
		FilterExcludeDomains: splitCSV(getEnv("FILTER_EXCLUDE_DOMAINS", "")),
		PrefilterMode:        strings.ToLower(getEnv("PREFILTER_MODE", "keywords")),
		PrefilterModel:       getEnv("PREFILTER_MODEL", "openai/gpt-oss-20b:free"),
		WebAddr:              getEnv("WEB_ADDR", ":8080"),
		WebUser:              os.Getenv("WEB_USER"),
		WebPass:              os.Getenv("WEB_PASS"),
	}

	// 45s: the history check is nearly free, so a short interval gives
	// near-real-time pickup without quota concerns.
	interval, err := time.ParseDuration(getEnv("POLL_INTERVAL", "45s"))
	if err != nil {
		return nil, fmt.Errorf("invalid POLL_INTERVAL: %w", err)
	}
	cfg.PollInterval = interval

	// Notifier selection: explicit NOTIFIER wins; otherwise pick whichever
	// channel is configured.
	cfg.Notifier = strings.ToLower(os.Getenv("NOTIFIER"))
	if cfg.Notifier == "" {
		switch {
		case cfg.TelegramBotToken != "":
			cfg.Notifier = "telegram"
		case cfg.NtfyTopic != "":
			cfg.Notifier = "ntfy"
		default:
			cfg.Notifier = "none"
		}
	}

	return cfg, nil
}

// RequirePoller validates the fields the poller cannot run without.
func (c *Config) RequirePoller() error {
	var missing []string
	switch c.LLMProvider {
	case "gemini":
		if c.GeminiAPIKey == "" {
			missing = append(missing, "GEMINI_API_KEY")
		}
	case "openrouter":
		if c.OpenRouterAPIKey == "" {
			missing = append(missing, "OPENROUTER_API_KEY")
		}
	case "anthropic":
		if c.AnthropicAPIKey == "" {
			missing = append(missing, "ANTHROPIC_API_KEY")
		}
	default:
		return fmt.Errorf("invalid LLM_PROVIDER: %q (gemini|openrouter|anthropic)", c.LLMProvider)
	}
	if c.GmailCredentialsFile == "" {
		missing = append(missing, "GMAIL_CREDENTIALS_FILE")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required setting(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getEnvInt64(key string, def int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getEnvFloat(key string, def float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

// splitCSV parses a comma-separated list, trimming and lowercasing each item.
func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
