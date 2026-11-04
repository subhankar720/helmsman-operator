package access

import (
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
)

// TestSubjectFromSpec covers every row of the subject-parsing table in
// WO-1.1 Appendix A, plus an empty user: name (extra edge case).
func TestSubjectFromSpec(t *testing.T) {
	user := func(name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: "User", Name: name, APIGroup: "rbac.authorization.k8s.io"}
	}

	tests := []struct {
		name  string
		input string
		want  rbacv1.Subject
	}{
		{"user prefix", "user:alice@example.com", user("alice@example.com")},
		{"bare string is a user", "alice@example.com", user("alice@example.com")},
		{
			"service account",
			"serviceaccount:team-a/builder",
			rbacv1.Subject{Kind: "ServiceAccount", Name: "builder", Namespace: "team-a"},
		},
		{"service account without slash", "serviceaccount:no-slash", user("serviceaccount:no-slash")},
		{"service account without namespace", "serviceaccount:/name", user("serviceaccount:/name")},
		{"service account without name", "serviceaccount:ns/", user("serviceaccount:ns/")},
		{"empty user name", "user:", user("user:")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := subjectFromSpec(tc.input); got != tc.want {
				t.Errorf("subjectFromSpec(%q) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}
