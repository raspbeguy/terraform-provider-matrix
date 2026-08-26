package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix"
)

// roomStateServer resolves one alias and answers each state read from a script,
// keyed by event type. Anything not named answers 200.
type roomStateServer struct {
	t        *testing.T
	statuses map[string]int
	seen     []string
}

func (s *roomStateServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if strings.Contains(r.URL.Path, "/directory/room/") {
		s.seen = append(s.seen, "resolve")
		_, _ = w.Write([]byte(`{"room_id":"!room:example.com","servers":["example.com"]}`))
		return
	}

	idx := strings.Index(r.URL.Path, "/state/")
	if idx < 0 {
		s.t.Errorf("unexpected request to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	evtType := strings.TrimSuffix(r.URL.Path[idx+len("/state/"):], "/")
	s.seen = append(s.seen, evtType)

	if status, scripted := s.statuses[evtType]; scripted && status != http.StatusOK {
		w.WriteHeader(status)
		errcode := "M_UNKNOWN"
		switch status {
		case http.StatusForbidden:
			errcode = "M_FORBIDDEN"
		case http.StatusNotFound:
			errcode = "M_NOT_FOUND"
		}
		_, _ = w.Write([]byte(`{"errcode":"` + errcode + `","error":"server said no"}`))
		return
	}

	switch evtType {
	case "m.room.name":
		_, _ = w.Write([]byte(`{"name":"Ops"}`))
	case "m.room.topic":
		_, _ = w.Write([]byte(`{"topic":"On call"}`))
	case "m.room.avatar":
		_, _ = w.Write([]byte(`{"url":"mxc://example.com/abc"}`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

// readRoomData drives the data source against a scripted server and returns the
// resulting state and diagnostics.
func readRoomData(t *testing.T, statuses map[string]int) (tfsdk.State, *roomStateServer, diagnosticsRecorder) {
	t.Helper()
	handler := &roomStateServer{t: t, statuses: statuses}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	mcli, err := mautrix.NewClient(srv.URL, "@bot:example.com", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// No retries, so a scripted failure is answered once and the request list
	// stays readable.
	mcli.DefaultHTTPRetries = 0

	ds := &roomDataSource{client: &Client{MX: mcli}}
	ctx := context.Background()

	var schemaResp datasource.SchemaResponse
	ds.Schema(ctx, datasource.SchemaRequest{}, &schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", schemaResp.Diagnostics)
	}

	cfg := tfsdk.State{Schema: schemaResp.Schema}
	if diags := cfg.Set(ctx, &roomDataSourceModel{
		Alias:     types.StringValue("#team:example.com"),
		RoomID:    types.StringNull(),
		Servers:   types.ListNull(types.StringType),
		Name:      types.StringNull(),
		Topic:     types.StringNull(),
		AvatarURL: types.StringNull(),
	}); diags.HasError() {
		t.Fatalf("building config: %v", diags)
	}

	resp := datasource.ReadResponse{State: tfsdk.State{Schema: schemaResp.Schema}}
	ds.Read(ctx, datasource.ReadRequest{
		Config: tfsdk.Config(cfg),
	}, &resp)
	return resp.State, handler, diagnosticsRecorder{d: resp.Diagnostics}
}

func attrString(t *testing.T, st tfsdk.State, name string) types.String {
	t.Helper()
	var v types.String
	if diags := st.GetAttribute(context.Background(), path.Root(name), &v); diags.HasError() {
		t.Fatalf("reading %s: %v", name, diags)
	}
	return v
}

// TestRoomDataSourceReportsWhatItCannotRead is the regression test for issue
// #60.
//
// The three state reads discarded their error, so a gateway restart came back
// as a null name and a null topic. Those flow into whatever consumes them, and
// a plan that removes a room's name because of a transient failure looks exactly
// like one that removes it on purpose.
func TestRoomDataSourceReportsWhatItCannotRead(t *testing.T) {
	cases := []struct {
		name      string
		statuses  map[string]int
		wantError bool
		wantName  string // "" means null
	}{
		{
			name:     "every read succeeds",
			statuses: nil,
			wantName: "Ops",
		},
		{
			// The documented case: a homeserver will not show room state to
			// someone who is not in the room. Synapse answers 403 M_FORBIDDEN
			// from check_user_in_room_or_world_readable.
			name:     "a refusal leaves the attribute null",
			statuses: map[string]int{"m.room.name": http.StatusForbidden},
		},
		{
			// getState already absorbs this, so a room that sets no name is
			// not an error. The row pins that it stays that way.
			name:     "a room with no name event is not an error",
			statuses: map[string]int{"m.room.name": http.StatusNotFound},
		},
		{
			name:      "a gateway error is reported",
			statuses:  map[string]int{"m.room.name": http.StatusBadGateway},
			wantError: true,
		},
		{
			name:      "an exhausted rate limit is reported",
			statuses:  map[string]int{"m.room.name": http.StatusTooManyRequests},
			wantError: true,
		},
		{
			// Not only the first read is guarded.
			name:      "a failure on the second read is reported",
			statuses:  map[string]int{"m.room.topic": http.StatusInternalServerError},
			wantError: true,
		},
		{
			name:      "a failure on the third read is reported",
			statuses:  map[string]int{"m.room.avatar": http.StatusServiceUnavailable},
			wantError: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, _, rec := readRoomData(t, c.statuses)
			if got := rec.d.HasError(); got != c.wantError {
				t.Fatalf("HasError = %v, want %v (%v)", got, c.wantError, rec.d)
			}
			if c.wantError {
				// Nothing is written after a failed read, so no half-filled
				// object reaches a configuration that consumes it.
				if !st.Raw.IsNull() {
					t.Errorf("state was set despite a failed read")
				}
				return
			}
			name := attrString(t, st, "name")
			if c.wantName == "" {
				if !name.IsNull() {
					t.Errorf("name = %v, want null", name)
				}
				return
			}
			if name.ValueString() != c.wantName {
				t.Errorf("name = %q, want %q", name.ValueString(), c.wantName)
			}
		})
	}
}

// TestRoomDataSourceStopsAfterOneFailure keeps a single outage to a single
// diagnostic. All three reads hit the same homeserver, so without the guard one
// gateway restart reports the same failure three times.
func TestRoomDataSourceStopsAfterOneFailure(t *testing.T) {
	st, server, rec := readRoomData(t, map[string]int{
		"m.room.name":   http.StatusBadGateway,
		"m.room.topic":  http.StatusBadGateway,
		"m.room.avatar": http.StatusBadGateway,
	})
	if !rec.d.HasError() {
		t.Fatal("want an error")
	}
	if len(rec.d) != 1 {
		t.Errorf("got %d diagnostics, want 1: %v", len(rec.d), rec.d)
	}
	if !strings.Contains(rec.d[0].Summary(), "m.room.name") {
		t.Errorf("the error must name the event that failed; got %q", rec.d[0].Summary())
	}
	// The reads after the failure are not attempted at all.
	if want := []string{"resolve", "m.room.name"}; strings.Join(server.seen, ",") != strings.Join(want, ",") {
		t.Errorf("requests = %v, want %v", server.seen, want)
	}
	if !st.Raw.IsNull() {
		t.Errorf("state was set despite a failed read")
	}
}
