package provider

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-testing/helper/acctest"
	tftest "github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// aliasServer answers directory requests from a script and records what
// arrived. The recorded methods are the point: an error alone does not say
// whether Update put back the mapping it removed.
type aliasServer struct {
	t       *testing.T
	replies []int    // status per request, in order; 200 beyond the list
	seen    []string // "METHOD" per request
}

func (s *aliasServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.Contains(r.URL.Path, "/directory/room/") {
		s.t.Errorf("unexpected request to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	status := http.StatusOK
	if len(s.seen) < len(s.replies) {
		status = s.replies[len(s.seen)]
	}
	s.seen = append(s.seen, r.Method)

	w.Header().Set("Content-Type", "application/json")
	if status != http.StatusOK {
		w.WriteHeader(status)
		errcode := "M_UNKNOWN"
		if status == http.StatusNotFound {
			errcode = "M_NOT_FOUND"
		}
		_, _ = w.Write([]byte(`{"errcode":"` + errcode + `","error":"server said no"}`))
		return
	}
	_, _ = w.Write([]byte(`{"room_id":"!resolved:example.com","servers":["example.com"]}`))
}

// aliasHarness wires the resource to a scripted server and builds the framework
// values its methods need.
type aliasHarness struct {
	res    *roomAliasResource
	server *aliasServer
	close  func()
}

func newAliasHarness(t *testing.T, replies ...int) *aliasHarness {
	t.Helper()
	handler := &aliasServer{t: t, replies: replies}
	srv := httptest.NewServer(handler)

	mcli, err := mautrix.NewClient(srv.URL, "@bot:example.com", "token")
	if err != nil {
		srv.Close()
		t.Fatalf("NewClient: %v", err)
	}
	// No retries, so the recorded request list is exactly what the resource
	// asked for.
	mcli.DefaultHTTPRetries = 0

	return &aliasHarness{
		res:    &roomAliasResource{client: &Client{MX: mcli}},
		server: handler,
		close:  srv.Close,
	}
}

func (h *aliasHarness) resourceSchema(t *testing.T) tfsdk.State {
	t.Helper()
	var resp resource.SchemaResponse
	h.res.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return tfsdk.State{Schema: resp.Schema}
}

func (h *aliasHarness) state(t *testing.T, m roomAliasModel) tfsdk.State {
	t.Helper()
	st := h.resourceSchema(t)
	if diags := st.Set(context.Background(), &m); diags.HasError() {
		t.Fatalf("building state: %v", diags)
	}
	return st
}

func aliasModel(alias, roomID string) roomAliasModel {
	return roomAliasModel{
		ID:     types.StringValue(alias),
		Alias:  types.StringValue(alias),
		RoomID: types.StringValue(roomID),
	}
}

// TestAliasReadKeepsWhatItCannotCheck is the regression test for issue #59.
//
// Read used to drop the resource on any error at all. The next plan then
// proposed a create, which failed with M_ROOM_IN_USE because the alias was
// there the whole time, and recovery was a manual import.
func TestAliasReadKeepsWhatItCannotCheck(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantRemove bool
		wantError  bool
	}{
		{
			// Synapse answers an unknown alias with 404 M_NOT_FOUND, in
			// directory.py. The alias really is gone.
			name: "a 404 means the alias is gone", status: http.StatusNotFound, wantRemove: true,
		},
		{
			// Synapse answers a federated lookup it could not complete with
			// 502 "Failed to fetch alias". The alias may well exist.
			name: "a 502 means we could not tell", status: http.StatusBadGateway, wantError: true,
		},
		{name: "a 500 means we could not tell", status: http.StatusInternalServerError, wantError: true},
		{name: "a rate limit means we could not tell", status: http.StatusTooManyRequests, wantError: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newAliasHarness(t, c.status)
			defer h.close()

			ctx := context.Background()
			prior := h.state(t, aliasModel("#team:example.com", "!room:example.com"))
			resp := resource.ReadResponse{State: prior}
			h.res.Read(ctx, resource.ReadRequest{State: prior}, &resp)

			if got := resp.Diagnostics.HasError(); got != c.wantError {
				t.Fatalf("HasError = %v, want %v (%v)", got, c.wantError, resp.Diagnostics)
			}
			if got := resp.State.Raw.IsNull(); got != c.wantRemove {
				t.Errorf("resource removed = %v, want %v. A Read must never drop a resource it "+
					"could not check (issue #59)", got, c.wantRemove)
			}
		})
	}
}

// TestAliasUpdateRestoresWhatItRemoved covers the recovery that was missing.
//
// An alias maps to one room, so delete then create is the only order available.
// If the create step fails, the alias no longer exists while state still
// describes the old mapping. The recorded request list is the assertion: an
// error alone does not say whether the mapping came back.
func TestAliasUpdateRestoresWhatItRemoved(t *testing.T) {
	cases := []struct {
		name      string
		replies   []int
		wantSeen  []string
		wantError bool
		wantIn    string
	}{
		{
			name:     "the happy path moves the alias",
			replies:  []int{http.StatusOK, http.StatusOK},
			wantSeen: []string{"DELETE", "PUT"},
		},
		{
			name:      "a failed create puts the old mapping back",
			replies:   []int{http.StatusOK, http.StatusForbidden, http.StatusOK},
			wantSeen:  []string{"DELETE", "PUT", "PUT"},
			wantError: true, wantIn: "restored, so nothing changed",
		},
		{
			name:      "a failed restore says the alias is gone",
			replies:   []int{http.StatusOK, http.StatusForbidden, http.StatusForbidden},
			wantSeen:  []string{"DELETE", "PUT", "PUT"},
			wantError: true, wantIn: "could not be restored",
		},
		{
			// An alias that is already gone needs no deleting. Without this the
			// resource is stuck after a half-finished update: every later apply
			// fails on a delete that can never succeed.
			name:     "a missing alias does not stop the move",
			replies:  []int{http.StatusNotFound, http.StatusOK},
			wantSeen: []string{"DELETE", "PUT"},
		},
		{
			name:      "a refused delete is still an error",
			replies:   []int{http.StatusForbidden},
			wantSeen:  []string{"DELETE"},
			wantError: true, wantIn: "delete step",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newAliasHarness(t, c.replies...)
			defer h.close()

			ctx := context.Background()
			prior := h.state(t, aliasModel("#team:example.com", "!old:example.com"))
			planned := h.state(t, aliasModel("#team:example.com", "!new:example.com"))

			resp := resource.UpdateResponse{State: prior}
			h.res.Update(ctx, resource.UpdateRequest{
				Plan:  tfsdk.Plan(planned),
				State: prior,
			}, &resp)

			if got := resp.Diagnostics.HasError(); got != c.wantError {
				t.Fatalf("HasError = %v, want %v (%v)", got, c.wantError, resp.Diagnostics)
			}
			if strings.Join(h.server.seen, ",") != strings.Join(c.wantSeen, ",") {
				t.Errorf("requests = %v, want %v", h.server.seen, c.wantSeen)
			}
			if c.wantIn != "" {
				// The phrase can sit in either half of the diagnostic.
				said := resp.Diagnostics[0].Summary() + " " + resp.Diagnostics[0].Detail()
				if !strings.Contains(said, c.wantIn) {
					t.Errorf("the error must contain %q; got %q", c.wantIn, said)
				}
			}
		})
	}
}

func aliasContent(alias string, alt ...string) *event.CanonicalAliasEventContent {
	c := &event.CanonicalAliasEventContent{Alias: id.RoomAlias(alias)}
	for _, a := range alt {
		c.AltAliases = append(c.AltAliases, id.RoomAlias(a))
	}
	return c
}

func TestAdvertisesAlias(t *testing.T) {
	const target = "#team:example.com"
	cases := []struct {
		name  string
		canon *event.CanonicalAliasEventContent
		want  bool
	}{
		{name: "the canonical alias", canon: aliasContent(target), want: true},
		{name: "an alternate alias", canon: aliasContent("#main:example.com", "#other:example.com", target), want: true},
		{name: "neither", canon: aliasContent("#main:example.com", "#other:example.com")},
		{name: "no event at all", canon: nil},
		{name: "an empty event", canon: &event.CanonicalAliasEventContent{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := advertisesAlias(target, c.canon); got != c.want {
				t.Errorf("advertisesAlias = %v, want %v", got, c.want)
			}
		})
	}
}

// TestCanonicalAliasWarning pins when the warning is worth making.
//
// Synapse removes a deleted alias from m.room.canonical_alias itself, so a
// warning on every advertised alias would cry wolf. It is worth making only
// when the caller cannot send that event, where Synapse swallows the AuthError
// and deletes the alias anyway.
func TestCanonicalAliasWarning(t *testing.T) {
	const (
		alias  = "#team:example.com"
		caller = "@bot:example.com"
		roomID = "!room:example.com"
	)
	powerLevels := func(callerLevel int) *event.PowerLevelsEventContent {
		pl := &event.PowerLevelsEventContent{StateDefaultPtr: ptr(50)}
		pl.Users = map[id.UserID]int{id.UserID(caller): callerLevel}
		return pl
	}

	cases := []struct {
		name   string
		canon  *event.CanonicalAliasEventContent
		pl     *event.PowerLevelsEventContent
		caller string
		want   bool
	}{
		{
			name:  "advertised and the caller cannot send the event",
			canon: aliasContent(alias), pl: powerLevels(0), caller: caller, want: true,
		},
		{
			name:  "advertised as an alternate and the caller cannot send it",
			canon: aliasContent("#main:example.com", alias), pl: powerLevels(0), caller: caller, want: true,
		},
		{
			// The homeserver cleans up by itself, so there is nothing to say.
			name:  "advertised and the caller can send the event",
			canon: aliasContent(alias), pl: powerLevels(50), caller: caller,
		},
		{
			name:  "not advertised at all",
			canon: aliasContent("#main:example.com"), pl: powerLevels(0), caller: caller,
		},
		{name: "no power levels to judge by", canon: aliasContent(alias), caller: caller},
		{name: "no caller to judge", canon: aliasContent(alias), pl: powerLevels(0)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := canonicalAliasWarning(alias, c.caller, roomID, c.canon, c.pl)
			if (got != "") != c.want {
				t.Fatalf("warning = %q, want one = %v", got, c.want)
			}
			if !c.want {
				return
			}
			for _, want := range []string{alias, roomID, c.caller, "no longer resolves"} {
				if !strings.Contains(got, want) {
					t.Errorf("the warning must mention %q; got %q", want, got)
				}
			}
		})
	}
}

// TestRoomAliasGuardsAreWired reads the resource's own source.
//
// Every fix here is a branch that a happy-path test never reaches, so each one
// keeps passing after the line that makes it work is deleted. See issue #59.
func TestRoomAliasGuardsAreWired(t *testing.T) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "room_alias_resource.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	bodies := map[string]*ast.BlockStmt{}
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Body != nil {
			bodies[fn.Name.Name] = fn.Body
		}
	}

	// count reports how often a method names an identifier, or a selector when
	// the name carries a dot.
	count := func(method, want string) int {
		body, ok := bodies[method]
		if !ok {
			t.Fatalf("%s not found in room_alias_resource.go", method)
		}
		var n int
		ast.Inspect(body, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.Ident:
				if v.Name == want {
					n++
				}
			case *ast.SelectorExpr:
				if inner, ok := v.X.(*ast.SelectorExpr); ok && inner.Sel.Name+"."+v.Sel.Name == want {
					n++
				}
			}
			return true
		})
		return n
	}

	cases := []struct {
		method, want, why string
		least             int
	}{
		{"Read", "notFoundErr", "a Read could drop a resource it merely failed to check", 1},
		{"Update", "notFoundErr", "an alias that is already gone would stop the move for good", 1},
		{"Update", "CreateAlias", "a failed create would leave no alias at all", 2},
		{"ModifyPlan", "Raw.IsNull", "the check would run on plans that create nothing", 1},
		{"ModifyPlan", "canonicalAliasWarning", "nothing would warn about a room left advertising a dead alias", 1},
	}
	for _, c := range cases {
		if got := count(c.method, c.want); got < c.least {
			t.Errorf("%s names %s %d times, want at least %d, or %s (issue #59)",
				c.method, c.want, got, c.least, c.why)
		}
	}
}

// testAccAliasConfig points one alias at one of two rooms.
func testAccAliasConfig(alias, target string) string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    matrix = { source = "raspbeguy/matrix" }
  }
}

provider "matrix" {}

resource "matrix_room" "first" {
  name   = "tf-acc-alias-first"
  preset = "private_chat"
}

resource "matrix_room" "second" {
  name   = "tf-acc-alias-second"
  preset = "private_chat"
}

resource "matrix_room_alias" "test" {
  alias   = %[1]q
  room_id = matrix_room.%[2]s.id
}
`, alias, target)
}

// TestAccRoomAlias_Reassign covers the happy path of the delete-then-create
// move, which the unit tests only reach by breaking it. An alias maps to one
// room, so a reassignment has to remove the mapping before it makes the new one.
func TestAccRoomAlias_Reassign(t *testing.T) {
	testAccSkipUnlessAcc(t)
	localpart := acctest.RandomWithPrefix("tf-acc-alias")
	alias := "#" + localpart + ":" + homeserverFromMXID(testAccUserID(t))

	tftest.Test(t, tftest.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy:             testAccCleanupAlias(t, localpart),
		Steps: []tftest.TestStep{
			{
				Config: testAccAliasConfig(alias, "first"),
				Check: tftest.TestCheckResourceAttrPair(
					"matrix_room_alias.test", "room_id", "matrix_room.first", "id"),
			},
			{
				// The move. Delete then create, against a real homeserver.
				Config: testAccAliasConfig(alias, "second"),
				Check: tftest.TestCheckResourceAttrPair(
					"matrix_room_alias.test", "room_id", "matrix_room.second", "id"),
			},
			// The refresh must agree, so Read resolves the alias to the new room
			// rather than reporting drift or dropping the resource.
			{Config: testAccAliasConfig(alias, "second"), PlanOnly: true},
		},
	})
}
