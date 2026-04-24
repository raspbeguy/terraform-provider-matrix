package provider

import (
	"strings"
	"testing"

	"maunium.net/go/mautrix/event"
)

func TestServerACLSelfLockoutWarnings_WildcardAllowIsSafe(t *testing.T) {
	c := &event.ServerACLEventContent{Allow: []string{"*"}, Deny: []string{"evil.example"}}
	got := serverACLSelfLockoutWarnings("matrix.example.com", c)
	if len(got) != 0 {
		t.Fatalf("expected no warnings for allow=[*], got %v", got)
	}
}

func TestServerACLSelfLockoutWarnings_EmptyAllowIsSafe(t *testing.T) {
	// Empty allow per spec means "allow all" — should not warn.
	c := &event.ServerACLEventContent{Deny: []string{"evil.example"}}
	got := serverACLSelfLockoutWarnings("matrix.example.com", c)
	if len(got) != 0 {
		t.Fatalf("expected no warnings for empty allow, got %v", got)
	}
}

func TestServerACLSelfLockoutWarnings_DenyMatchesSelfLiteral(t *testing.T) {
	c := &event.ServerACLEventContent{Deny: []string{"matrix.example.com"}}
	got := serverACLSelfLockoutWarnings("matrix.example.com", c)
	if len(got) == 0 {
		t.Fatal("expected warning for literal deny of self")
	}
	if !strings.Contains(got[0], "lock you out") {
		t.Errorf("warning should mention lockout; got %q", got[0])
	}
}

func TestServerACLSelfLockoutWarnings_DenyMatchesSelfGlob(t *testing.T) {
	c := &event.ServerACLEventContent{Deny: []string{"matrix.*"}}
	got := serverACLSelfLockoutWarnings("matrix.example.com", c)
	if len(got) == 0 {
		t.Fatal("expected warning for glob deny matching self")
	}
}

func TestServerACLSelfLockoutWarnings_AllowExcludesSelf(t *testing.T) {
	c := &event.ServerACLEventContent{Allow: []string{"other.example.com", "friend.*"}}
	got := serverACLSelfLockoutWarnings("matrix.example.com", c)
	if len(got) == 0 {
		t.Fatal("expected warning when allow list excludes self")
	}
}

func TestServerACLSelfLockoutWarnings_AllowIncludesSelfViaGlob(t *testing.T) {
	c := &event.ServerACLEventContent{Allow: []string{"*.example.com"}}
	got := serverACLSelfLockoutWarnings("matrix.example.com", c)
	if len(got) != 0 {
		t.Fatalf("expected no warning when a glob in allow matches self, got %v", got)
	}
}

func TestServerACLSelfLockoutWarnings_EmptyHomeserverNoOp(t *testing.T) {
	c := &event.ServerACLEventContent{Deny: []string{"*"}}
	got := serverACLSelfLockoutWarnings("", c)
	if len(got) != 0 {
		t.Fatalf("expected no warnings with empty homeserver, got %v", got)
	}
}

func TestServerACLInvalidPatternWarnings_CatchesBadGlob(t *testing.T) {
	c := &event.ServerACLEventContent{
		Allow: []string{"ok.example.com", "matrix.[unterminated"},
		Deny:  []string{"*.spam.example", "[bad"},
	}
	got := serverACLInvalidPatternWarnings(c)
	if len(got) != 2 {
		t.Fatalf("expected 2 malformed-pattern warnings, got %d: %v", len(got), got)
	}
	joined := strings.Join(got, " | ")
	if !strings.Contains(joined, "matrix.[unterminated") {
		t.Errorf("expected warning for allow entry; got %q", joined)
	}
	if !strings.Contains(joined, "[bad") {
		t.Errorf("expected warning for deny entry; got %q", joined)
	}
}

func TestServerACLInvalidPatternWarnings_AllValidNoWarnings(t *testing.T) {
	c := &event.ServerACLEventContent{
		Allow: []string{"*", "matrix.org", "*.example.com"},
		Deny:  []string{"evil.example"},
	}
	if got := serverACLInvalidPatternWarnings(c); len(got) != 0 {
		t.Fatalf("expected no warnings for well-formed patterns, got %v", got)
	}
}

func TestHomeserverFromMXID(t *testing.T) {
	cases := []struct {
		mxid, want string
	}{
		{"@alice:matrix.example.com", "matrix.example.com"},
		{"@bob:foo.bar:8448", "foo.bar:8448"},
		{"no-colon-here", ""},
		{"", ""},
		{"@missing:", ""},
	}
	for _, tc := range cases {
		// Build a fake client with the mxid.
		// Client.MX is *mautrix.Client; we only need UserID for this helper.
		// Skipped in a full client constructor — inline the logic inputs only.
		got := homeserverFromMXID(tc.mxid)
		if got != tc.want {
			t.Errorf("homeserverFromMXID(%q) = %q, want %q", tc.mxid, got, tc.want)
		}
	}
}
