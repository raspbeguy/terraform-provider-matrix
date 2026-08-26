package provider

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"maunium.net/go/mautrix/event"
)

func TestServerACLSelfLockoutWarnings_WildcardAllowIsSafe(t *testing.T) {
	c := &event.ServerACLEventContent{Allow: []string{"*"}, Deny: []string{"evil.example"}}
	got := serverACLSelfLockoutWarnings("matrix.example.com", c)
	if len(got) != 0 {
		t.Fatalf("expected no warnings for allow=[*], got %v", got)
	}
}

func TestServerACLSelfLockoutWarnings_DenyMatchesSelfLiteral(t *testing.T) {
	c := &event.ServerACLEventContent{Deny: []string{"matrix.example.com"}}
	got := serverACLSelfLockoutWarnings("matrix.example.com", c)
	if len(got) == 0 {
		t.Fatal("expected warning for literal deny of self")
	}
	if !strings.Contains(got[0], "blocks your own server") {
		t.Errorf("the warning must say what the ACL does; got %q", got[0])
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

// TestGlobMatchHomeserver pins the glob against Synapse's, which is what decides
// whether a lockout is real. glob_to_regex in rust/src/push/utils.rs escapes
// every run between wildcards and compiles case-insensitively, so a bracket is a
// literal and case does not matter. path.Match, which this used to call, agreed
// with neither. See issue #61.
func TestGlobMatchHomeserver(t *testing.T) {
	cases := []struct {
		pattern, server string
		want            bool
	}{
		{"*", "example.com", true},
		{"*", "", true}, // "zero or more"
		{"example.com", "example.com", true},
		{"example.com", "example.org", false},
		{"*.example.com", "a.example.com", true},
		{"*.example.com", "example.com", false},
		{"?.example.com", "a.example.com", true},
		{"?.example.com", "ab.example.com", false},
		{"?", "", false}, // "exactly one"
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"?**?**?", "abcde", true}, // Synapse folds a wildcard run to .{3,}
		{"?**?**?", "ab", false},   // and three "?" still demand three characters
		{"*.example.com", "a.b.example.com", true},
		// A bracket is a literal, not a character class. This row is the missed
		// lockout: an IPv6 homeserver name denied by its exact name.
		{"[2001:db8::1]", "[2001:db8::1]", true},
		{"[2001:db8::1]", "x", false},
		{"evil[.example", "evil[.example", true},
		{"[bad", "[bad", true},
		// Synapse matches case-insensitively, so a deny entry in another case
		// still locks you out.
		{"Evil.Example", "evil.example", true},
		{"evil.example", "EVIL.EXAMPLE", true},
		{"*.EXAMPLE.com", "a.example.COM", true},
		{"", "", true},
		{"", "a", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+" vs "+tc.server, func(t *testing.T) {
			if got := globMatchHomeserver(tc.pattern, tc.server); got != tc.want {
				t.Errorf("globMatchHomeserver(%q, %q) = %v, want %v", tc.pattern, tc.server, got, tc.want)
			}
		})
	}
}

// aclModel builds the model the deny-all check reads.
func aclModel(allow types.Set) *serverACLModel {
	return &serverACLModel{Allow: allow, Deny: types.SetNull(types.StringType)}
}

func aclAllow(values ...string) types.Set {
	elems := make([]attr.Value, len(values))
	for i, v := range values {
		elems[i] = types.StringValue(v)
	}
	return types.SetValueMust(types.StringType, elems)
}

// synapseOracle builds the regex the way Synapse's glob_to_regex does: escape
// every run between wildcards, "*" becomes .{n,}, "?" becomes .{n}, anchored
// whole and case-insensitive. See rust/src/push/utils.rs. It is deliberately a
// second implementation of the rule, so that it can disagree with the first.
func synapseOracle(t *testing.T, glob string) *regexp.Regexp {
	t.Helper()
	var b strings.Builder
	b.WriteString(`(?is)\A`)
	i := 0
	for i < len(glob) {
		if glob[i] == '*' || glob[i] == '?' {
			j, q, star := i, 0, false
			for j < len(glob) && (glob[j] == '*' || glob[j] == '?') {
				if glob[j] == '?' {
					q++
				} else {
					star = true
				}
				j++
			}
			if star {
				fmt.Fprintf(&b, ".{%d,}", q)
			} else {
				fmt.Fprintf(&b, ".{%d}", q)
			}
			i = j
			continue
		}
		j := i
		for j < len(glob) && glob[j] != '*' && glob[j] != '?' {
			j++
		}
		b.WriteString(regexp.QuoteMeta(glob[i:j]))
		i = j
	}
	b.WriteString(`\z`)
	return regexp.MustCompile(b.String())
}

// TestGlobMatchHomeserverMatchesSynapse compares the matcher against an
// independent expression of the same rule over every short pattern and name
// built from an alphabet that includes both wildcards, a bracket and two cases.
// A table of hand-picked rows cannot catch a wrong backtrack; this can.
func TestGlobMatchHomeserverMatchesSynapse(t *testing.T) {
	alphabet := []string{"", "a", "b", "*", "?", ".", "[", "]", "A", "-"}
	var pats, servers []string
	for _, x := range alphabet {
		for _, y := range alphabet {
			for _, z := range alphabet {
				pats = append(pats, x+y+z)
			}
		}
	}
	for _, x := range []string{"", "a", "b", "A", ".", "[", "]", "-"} {
		for _, y := range []string{"", "a", "b", "A", ".", "["} {
			for _, z := range []string{"", "a", "b", "["} {
				servers = append(servers, x+y+z)
			}
		}
	}
	bad := 0
	for _, p := range pats {
		re := synapseOracle(t, p)
		for _, s := range servers {
			want := re.MatchString(s)
			got := globMatchHomeserver(p, s)
			if got != want {
				if bad < 10 {
					t.Errorf("glob(%q, %q) = %v, Synapse oracle says %v", p, s, got, want)
				}
				bad++
			}
		}
	}
	t.Logf("compared %d pattern/server pairs, %d disagreements", len(pats)*len(servers), bad)
}

// TestServerACLDenyAllDiag covers the rule the resource used to document
// backwards. A homeserver checks deny, then allow, then rejects what matched
// neither, so an empty allow list denies every server. See issue #57.
func TestServerACLDenyAllDiag(t *testing.T) {
	cases := []struct {
		name      string
		model     *serverACLModel
		wantError bool
	}{
		{
			// The natural way to write "block one server", and the reported bug.
			name:      "an absent allow denies everyone",
			model:     aclModel(types.SetNull(types.StringType)),
			wantError: true,
		},
		{
			name:      "an explicitly empty allow denies everyone",
			model:     aclModel(aclAllow()),
			wantError: true,
		},
		{
			// A list computed from another resource. Refusing this would break
			// a valid configuration, so the check has to stand down.
			name:  "an unknown allow is not judged yet",
			model: aclModel(types.SetUnknown(types.StringType)),
		},
		{name: "a wildcard allow is fine", model: aclModel(aclAllow("*"))},
		{name: "a named allow is fine", model: aclModel(aclAllow("matrix.example.com"))},
		{name: "no model at all", model: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serverACLDenyAllDiag("matrix.example.com", tc.model)
			if got.HasError() != tc.wantError {
				t.Fatalf("HasError = %v, want %v (%v)", got.HasError(), tc.wantError, got)
			}
			if !tc.wantError {
				return
			}
			detail := got[0].Detail()
			if !strings.Contains(detail, "matrix.example.com") {
				t.Errorf("the error must name the caller's homeserver; got %q", detail)
			}
			// The old text claimed the caller could not send a corrective ACL.
			// Synapse checks the ACL only on inbound federation, so that is
			// wrong, and the message must not say it.
			if strings.Contains(detail, "only a homeserver admin") {
				t.Errorf("the error repeats a claim Synapse contradicts; got %q", detail)
			}
		})
	}
}

// TestServerACLDenyAllDiag_NoHomeserver keeps the message readable when the
// provider cannot work out its own server name.
func TestServerACLDenyAllDiag_NoHomeserver(t *testing.T) {
	got := serverACLDenyAllDiag("", aclModel(types.SetNull(types.StringType)))
	if !got.HasError() {
		t.Fatal("an empty allow list must be refused whether or not the homeserver is known")
	}
	if strings.Contains(got[0].Detail(), "homeserver  ") {
		t.Errorf("the message has a gap where the server name would be: %q", got[0].Detail())
	}
}

// TestServerACLSelfLockoutWarnings_IPLiteralHomeserver covers the fourth lockout
// route in the same guard. Synapse rejects a bracketed name, or one that parses
// as IPv4, before it reads either list.
func TestServerACLSelfLockoutWarnings_IPLiteralHomeserver(t *testing.T) {
	c := &event.ServerACLEventContent{Allow: []string{"*"}, AllowIPLiterals: false}
	for _, hs := range []string{"[2001:db8::1]", "1.2.3.4"} {
		got := serverACLSelfLockoutWarnings(hs, c)
		if len(got) == 0 {
			t.Errorf("homeserver %q is an IP literal and allow_ip_literals is false, want a warning", hs)
		}
	}
	if got := serverACLSelfLockoutWarnings("matrix.example.com", c); len(got) != 0 {
		t.Errorf("a name is not an IP literal, want no warning, got %v", got)
	}
	allowed := &event.ServerACLEventContent{Allow: []string{"*"}, AllowIPLiterals: true}
	if got := serverACLSelfLockoutWarnings("1.2.3.4", allowed); len(got) != 0 {
		t.Errorf("allow_ip_literals is true, want no warning, got %v", got)
	}
}

// TestServerACLSelfLockoutWarnings_DenyMatchesSelfInAnotherCase is the case half
// of issue #61. Synapse folds case, so this locks the caller out.
func TestServerACLSelfLockoutWarnings_DenyMatchesSelfInAnotherCase(t *testing.T) {
	c := &event.ServerACLEventContent{Allow: []string{"*"}, Deny: []string{"Matrix.Example.COM"}}
	got := serverACLSelfLockoutWarnings("matrix.example.com", c)
	if len(got) == 0 {
		t.Fatal("a deny entry in another case still matches on the homeserver, want a warning")
	}
}

func TestIsIPLiteral(t *testing.T) {
	cases := []struct {
		server string
		want   bool
	}{
		{"[2001:db8::1]", true},
		{"1.2.3.4", true},
		{"matrix.example.com", false},
		{"", false},
		// Synapse parses the whole name, so a port stops it being a literal.
		{"1.2.3.4:8448", false},
		// Rust's Ipv4Addr rejects the IPv4-mapped form and Go's ParseIP accepts
		// it, so this row pins the stricter reading.
		{"::ffff:1.2.3.4", false},
	}
	for _, tc := range cases {
		if got := isIPLiteral(tc.server); got != tc.want {
			t.Errorf("isIPLiteral(%q) = %v, want %v", tc.server, got, tc.want)
		}
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

// testAccServerACLConfig builds a room and an ACL for it. An empty allow list
// is written by leaving the attribute out, which is how a practitioner writes
// "block one server" and how issue #57 was reached.
func testAccServerACLConfig(allow string) string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    matrix = { source = "raspbeguy/matrix" }
  }
}

provider "matrix" {}

resource "matrix_room" "test" {
  name   = "tf-acc-server-acl"
  topic  = "Managed by the acceptance suite"
  preset = "private_chat"
}

resource "matrix_room_server_acl" "test" {
  room_id = matrix_room.test.id
  deny    = ["evil.example"]
  %[1]s
}
`, allow)
}

// TestAccServerACL_DenyAllIsRefused is the end-to-end guard for issue #57. The
// plan must fail before anything is sent, so the test never publishes an ACL
// that denies every server.
//
// The second step proves the guard lets a correct ACL through, because a check
// that refuses everything would pass the first step on its own.
func TestAccServerACL_DenyAllIsRefused(t *testing.T) {
	testAccSkipUnlessAcc(t)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config:      testAccServerACLConfig(""),
				ExpectError: regexp.MustCompile(`allow list denies every server`),
			},
			{
				Config: testAccServerACLConfig(`allow = ["*"]`),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("matrix_room_server_acl.test",
						tfjsonpath.New("allow"), knownvalue.SetExact([]knownvalue.Check{knownvalue.StringExact("*")})),
				},
			},
			// The same configuration must plan clean afterwards. A guard that
			// fires on a refresh would make the resource unusable.
			{Config: testAccServerACLConfig(`allow = ["*"]`), PlanOnly: true},
		},
	})
}
