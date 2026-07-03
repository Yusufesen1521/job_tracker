package gmail

import "testing"

func TestFilterAllow(t *testing.T) {
	f := NewFilter(
		[]string{"application", "mülakat", "interview"},
		[]string{"newsletter.com"},
	)

	tests := []struct {
		name string
		msg  Message
		want bool
	}{
		{"keyword in subject", Message{Subject: "Your application was received"}, true},
		{"turkish keyword in body", Message{Body: "Mülakat daveti"}, true},
		{"excluded domain", Message{From: "news@newsletter.com", Subject: "interview"}, false},
		{"no keyword", Message{Subject: "Lunch tomorrow?"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := f.Allow(tt.msg)
			if got != tt.want {
				t.Fatalf("Allow=%v, want %v (reason: %s)", got, tt.want, reason)
			}
		})
	}
}

func TestFilterVerdict(t *testing.T) {
	f := NewFilter([]string{"application"}, []string{"newsletter.com"})

	tests := []struct {
		name string
		msg  Message
		want FilterVerdict
	}{
		{"keyword match passes", Message{Subject: "application received"}, Pass},
		{"excluded domain wins over keyword", Message{From: "x@newsletter.com", Subject: "application"}, ExcludedDomain},
		{"no keyword", Message{Subject: "Lunch?"}, NoKeyword},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := f.Verdict(tt.msg)
			if got != tt.want {
				t.Fatalf("Verdict=%v, want %v (reason: %s)", got, tt.want, reason)
			}
		})
	}
}

func TestFilterEmptyKeywordsAllowsAll(t *testing.T) {
	f := NewFilter(nil, nil)
	if ok, _ := f.Allow(Message{Subject: "anything"}); !ok {
		t.Fatalf("every mail should pass when the keyword list is empty")
	}
}
