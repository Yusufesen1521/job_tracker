package gmail

import "strings"

// Filter performs a cheap pre-screening of mails before the LLM.
// Goal: avoid sending irrelevant mail to the LLM, saving tokens/cost.
// Both the keywords and the excluded domains are configurable.
type Filter struct {
	keywords       []string // lowercase; at least one must appear in subject/body
	excludeDomains []string // lowercase; senders from these domains are dropped
}

// NewFilter creates a configurable pre-filter.
func NewFilter(keywords, excludeDomains []string) *Filter {
	return &Filter{keywords: keywords, excludeDomains: excludeDomains}
}

// FilterVerdict is one of the three possible pre-filter outcomes. In hybrid
// mode a NoKeyword verdict is not a hard reject; a cheap LLM gets a second look.
type FilterVerdict int

const (
	// Pass means the mail should be sent to the LLM classifier.
	Pass FilterVerdict = iota
	// ExcludedDomain means the sender is blacklisted; hard reject, no LLM.
	ExcludedDomain
	// NoKeyword means no keyword matched; in hybrid mode a cheap LLM is consulted.
	NoKeyword
)

// Verdict returns the detailed pre-filter outcome for a mail.
// reason is a human-readable justification for logging/debugging only.
func (f *Filter) Verdict(msg Message) (v FilterVerdict, reason string) {
	from := strings.ToLower(msg.From)

	// 1) Excluded domains.
	for _, d := range f.excludeDomains {
		if d != "" && strings.Contains(from, d) {
			return ExcludedDomain, "sender domain is excluded: " + d
		}
	}

	// 2) An empty keyword list disables the filter (everything passes).
	if len(f.keywords) == 0 {
		return Pass, "keyword filter disabled"
	}

	haystack := strings.ToLower(msg.Subject + " " + msg.Snippet + " " + msg.Body)
	for _, kw := range f.keywords {
		if kw != "" && strings.Contains(haystack, kw) {
			return Pass, "keyword matched: " + kw
		}
	}
	return NoKeyword, "no keyword matched"
}

// Allow reports whether the mail is worth sending to the LLM (binary summary
// of Verdict; NoKeyword counts as a reject).
func (f *Filter) Allow(msg Message) (allow bool, reason string) {
	v, reason := f.Verdict(msg)
	return v == Pass, reason
}
