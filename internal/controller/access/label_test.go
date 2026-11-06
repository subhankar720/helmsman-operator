package access

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// TestSanitizeLabel checks that every subject becomes a valid Kubernetes label
// value (at most 63 characters; empty, or starting and ending with an
// alphanumeric; only letters, digits, '-', '_' and '.'), and that short
// subjects stay readable and unchanged in meaning.
func TestSanitizeLabel(t *testing.T) {
	long70 := "user:" + strings.Repeat("a", 45) + "@example-corporation.com" // 5+45+20 = 70 chars
	tests := []struct {
		name  string
		input string
		want  string // "" = only check validity (and the extra properties below)
	}{
		{"user email (unchanged behaviour)", "user:alice@example.com", "user-alice-example.com"},
		{"service account", "serviceaccount:team-a/builder", "serviceaccount-team-a-builder"},
		{"already valid", "alice", "alice"},
		{"leading non-alphanumeric", "-alice", "alice"},
		{"trailing non-alphanumeric", "user:bob@example.", "user-bob-example"},
		{"only invalid characters", "@@@", ""},
		{"exactly 63 characters", strings.Repeat("b", 63), strings.Repeat("b", 63)},
		{"64 characters", strings.Repeat("c", 64), ""},
		{"70+ character email subject", long70, ""},
		{"long subject whose cut point is a dash", strings.Repeat("d", 53) + "@@@@@@@@@@@@@@@", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeLabel(tt.input)
			if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
				t.Fatalf("sanitizeLabel(%q) = %q (len %d) is not a valid label value: %v", tt.input, got, len(got), errs)
			}
			if tt.want != "" && got != tt.want {
				t.Fatalf("sanitizeLabel(%q) = %q, want %q", tt.input, got, tt.want)
			}
			if len(tt.input) > 63 && len(got) > 0 {
				// readable: keeps the start of the subject
				if !strings.HasPrefix(got, sanitizeChars(tt.input)[:20]) {
					t.Fatalf("sanitizeLabel(%q) = %q does not keep the readable prefix", tt.input, got)
				}
			}
		})
	}

	t.Run("long subjects sharing a prefix stay distinct", func(t *testing.T) {
		a := sanitizeLabel(long70)
		b := sanitizeLabel(long70 + "x")
		if a == b {
			t.Fatalf("two different long subjects map to the same label %q", a)
		}
	})
	t.Run("deterministic", func(t *testing.T) {
		first, second := sanitizeLabel(long70), sanitizeLabel(long70)
		if first != second {
			t.Fatal("sanitizeLabel is not deterministic")
		}
	})
}

// sanitizeChars is the character replacement alone (test helper).
func sanitizeChars(s string) string {
	b := []byte(s)
	for i, c := range b {
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_'
		if !ok {
			b[i] = '-'
		}
	}
	return string(b)
}
