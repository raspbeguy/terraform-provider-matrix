package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/id"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"maunium.net/go/mautrix"
)

// diagnosticsRecorder is a tiny holder so a test can pass a *diag.Diagnostics.
type diagnosticsRecorder struct{ d diag.Diagnostics }

func (r *diagnosticsRecorder) ptr() *diag.Diagnostics { return &r.d }

func httpErr(status int, code string) error {
	e := mautrix.HTTPError{Response: &http.Response{StatusCode: status}}
	if code != "" {
		e.RespError = &mautrix.RespError{ErrCode: code, Err: "server said no"}
	}
	return e
}

// TestNotFoundErr pins both branches of the predicate that decides whether a
// destroy may treat its target as already gone.
//
// The status-code branch is load-bearing and was unpinned. Synapse answers a
// purged room with 404 and errcode M_UNKNOWN, not M_NOT_FOUND, so anyone
// simplifying notFoundErr to trust the errcode alone would make destroy
// impossible for such a room while the suite stayed green. Issue #45 tightened
// destroy to fail on a refusal, so this is the edge that must not swing too far.
func TestNotFoundErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "Synapse purged room: 404 with M_UNKNOWN", err: httpErr(http.StatusNotFound, "M_UNKNOWN"), want: true},
		{name: "404 with no errcode", err: httpErr(http.StatusNotFound, ""), want: true},
		{name: "M_NOT_FOUND at another status", err: httpErr(http.StatusBadRequest, "M_NOT_FOUND"), want: true},
		{name: "a refusal is not a missing thing", err: httpErr(http.StatusForbidden, "M_FORBIDDEN")},
		{name: "a gateway error is not a missing thing", err: httpErr(http.StatusBadGateway, "M_UNKNOWN")},
		{name: "a plain error is not a missing thing", err: errors.New("connection refused")},
		{name: "no error at all", err: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := notFoundErr(c.err); got != c.want {
				t.Errorf("notFoundErr = %v, want %v", got, c.want)
			}
		})
	}
}

// TestForbiddenErr pins what counts as "the homeserver will not show me this",
// which is the one state-read failure the room data source may answer with a
// null. Everything else it must report, or a plan removes a name because a
// gateway was restarting. See issue #60.
func TestForbiddenErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			// What Synapse answers a non-member reading room state, from
			// check_user_in_room_or_world_readable.
			name: "not in the room: 403 with M_FORBIDDEN",
			err:  httpErr(http.StatusForbidden, "M_FORBIDDEN"), want: true,
		},
		{name: "403 with another errcode", err: httpErr(http.StatusForbidden, "M_GUEST_ACCESS_FORBIDDEN"), want: true},
		{name: "403 with no errcode", err: httpErr(http.StatusForbidden, ""), want: true},
		{name: "M_FORBIDDEN at another status", err: httpErr(http.StatusBadRequest, "M_FORBIDDEN"), want: true},
		{name: "a missing event is not a refusal", err: httpErr(http.StatusNotFound, "M_NOT_FOUND")},
		{name: "a gateway error is not a refusal", err: httpErr(http.StatusBadGateway, "M_UNKNOWN")},
		{name: "a rate limit is not a refusal", err: httpErr(http.StatusTooManyRequests, "M_LIMIT_EXCEEDED")},
		{name: "a transport failure is not a refusal", err: errors.New("connection reset")},
		{name: "no error at all", err: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := forbiddenErr(c.err); got != c.want {
				t.Errorf("forbiddenErr = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAmbiguousErr pins which failures leave it unknown whether the homeserver
// did the work. Only those may carry the warning that a room might exist. See
// issue #55: the warning used to go out with every create failure, including a
// rate limit, which proves the opposite.
func TestAmbiguousErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "a transport failure never reached a server", err: errors.New("connection reset"), want: true},
		{name: "a request with no response at all", err: mautrix.HTTPError{Message: "timeout"}, want: true},
		{name: "500 from the homeserver", err: httpErr(http.StatusInternalServerError, "M_UNKNOWN"), want: true},
		{name: "502 from a gateway", err: httpErr(http.StatusBadGateway, ""), want: true},
		{name: "503 from a load balancer", err: httpErr(http.StatusServiceUnavailable, ""), want: true},
		{name: "504 from a gateway", err: httpErr(http.StatusGatewayTimeout, "M_UNKNOWN"), want: true},
		{
			// mautrix reports a body it could not read or parse with the
			// successful response attached. The homeserver made the room and
			// nothing knows its id.
			name: "a success whose body could not be parsed", err: httpErr(http.StatusOK, ""), want: true,
		},
		{name: "a redirect nobody followed", err: httpErr(http.StatusFound, ""), want: true},
		{name: "a rate limit was refused before any work", err: httpErr(http.StatusTooManyRequests, "M_LIMIT_EXCEEDED")},
		{name: "a forbidden is a refusal", err: httpErr(http.StatusForbidden, "M_FORBIDDEN")},
		{name: "a bad request is a refusal", err: httpErr(http.StatusBadRequest, "M_UNSUPPORTED_ROOM_VERSION")},
		{name: "a not found is a refusal", err: httpErr(http.StatusNotFound, "M_NOT_FOUND")},
		{
			// The shape retryOnRateLimit returns when the wait is cancelled.
			// The cancellation alone would read as ambiguous, so the refusal
			// has to travel with it.
			name: "a cancelled backoff still carries its refusal",
			err:  errors.Join(context.Canceled, httpErr(http.StatusTooManyRequests, "M_LIMIT_EXCEEDED")),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ambiguousErr(c.err); got != c.want {
				t.Errorf("ambiguousErr = %v, want %v", got, c.want)
			}
		})
	}
}

// TestFailedDestroy covers the rule every Delete now shares. Before issue #45
// five of the six resources downgraded any destroy failure to a warning, so a
// destroy that the homeserver refused still reported success and exit status 0,
// and the resource left state with the work outstanding.
func TestFailedDestroy(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantError bool
	}{
		{name: "nothing went wrong", err: nil},
		// The shape a homeserver really sends for a purged room: 404 with an
		// errcode that is not M_NOT_FOUND. It reaches the tolerance only through
		// the status-code branch of notFoundErr. See issue #52.
		{name: "a 404 with any errcode means it is already gone", err: httpErr(http.StatusNotFound, "M_UNKNOWN")},
		{name: "a 404 with no errcode means it is already gone", err: httpErr(http.StatusNotFound, "")},
		{name: "M_NOT_FOUND means it is already gone", err: httpErr(http.StatusBadRequest, "M_NOT_FOUND")},
		{name: "a refusal is a failure", err: httpErr(http.StatusForbidden, "M_FORBIDDEN"), wantError: true},
		{name: "a rate limit is a failure", err: httpErr(http.StatusTooManyRequests, "M_LIMIT_EXCEEDED"), wantError: true},
		{name: "a plain error is a failure", err: errors.New("connection refused"), wantError: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var diags diagnosticsRecorder
			failedDestroy(diags.ptr(), "Cleanup failed", c.err)
			if diags.ptr().HasError() != c.wantError {
				t.Fatalf("HasError = %v, want %v: %v", diags.ptr().HasError(), c.wantError, diags.ptr())
			}
			if !c.wantError {
				if len(*diags.ptr()) != 0 {
					t.Errorf("want no diagnostics at all, got %v", diags.ptr())
				}
				return
			}
			d := (*diags.ptr())[0]
			if d.Summary() != "Cleanup failed" {
				t.Errorf("summary = %q, want the caller's own summary", d.Summary())
			}
			if !strings.Contains(d.Detail(), "terraform state rm") {
				t.Errorf("the detail must name the escape hatch; got %q", d.Detail())
			}
			if !strings.Contains(d.Detail(), c.err.Error()) {
				t.Errorf("the detail must keep the homeserver's message; got %q", d.Detail())
			}
		})
	}
}

// createRoomServer counts creates and answers each one from a script.
type createRoomServer struct {
	t        *testing.T
	statuses []int
	seen     int
	// brokenBody cuts the success body short. The room exists, and the client
	// never learns its id. mautrix reports that with the 2xx response attached.
	brokenBody bool
}

func (s *createRoomServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/createRoom") {
		s.t.Errorf("unexpected request to %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	status := http.StatusOK
	if s.seen < len(s.statuses) {
		status = s.statuses[s.seen]
	}
	s.seen++
	switch status {
	case http.StatusOK:
		w.Header().Set("Content-Type", "application/json")
		if s.brokenBody {
			_, _ = w.Write([]byte(`{"room_id":`))
			return
		}
		_, _ = w.Write([]byte(`{"room_id":"!created:example.com"}`))
	case http.StatusTooManyRequests:
		w.Header().Set("Retry-After", "0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errcode":"M_LIMIT_EXCEEDED","error":"Too Many Requests","retry_after_ms":0}`))
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errcode":"M_UNKNOWN","error":"gateway said no"}`))
	}
}

// invitesSet turns a list of user ids into the attribute value createRoomLike
// reads, or a null set when there are none.
func invitesSet(t *testing.T, invites []string) types.Set {
	t.Helper()
	if len(invites) == 0 {
		return types.SetNull(types.StringType)
	}
	values := make([]attr.Value, len(invites))
	for i, u := range invites {
		values[i] = types.StringValue(u)
	}
	set, diags := types.SetValue(types.StringType, values)
	if diags.HasError() {
		t.Fatalf("building the invite set: %v", diags)
	}
	return set
}

// createRoomOnce drives createRoomLike against a scripted server and reports how
// many creates arrived.
func createRoomOnce(t *testing.T, statuses ...int) (id.RoomID, diag.Diagnostics, int) {
	t.Helper()
	return runCreateRoom(t, &createRoomServer{t: t, statuses: statuses})
}

// runCreateRoom drives createRoomLike against a scripted server. Any invites
// given go into the create request, which changes what a refusal means.
func runCreateRoom(t *testing.T, handler *createRoomServer, invites ...string) (id.RoomID, diag.Diagnostics, int) {
	t.Helper()
	srv := httptest.NewServer(handler)
	defer srv.Close()

	mcli, err := mautrix.NewClient(srv.URL, "@bot:example.com", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	mcli.DefaultHTTPRetries = matrixHTTPRetries

	// Every optional attribute is null, so this path makes exactly one request
	// and the count is unambiguous.
	m := &baseRoomModel{
		Name:              types.StringValue("tf-unit"),
		Topic:             types.StringNull(),
		AvatarURL:         types.StringNull(),
		Preset:            types.StringNull(),
		Visibility:        types.StringNull(),
		RoomVersion:       types.StringNull(),
		RoomAliasName:     types.StringNull(),
		InitialInvites:    invitesSet(t, invites),
		HistoryVisibility: types.StringNull(),
	}
	var diags diag.Diagnostics
	roomID := createRoomLike(context.Background(), &Client{MX: mcli}, m, false, false, false, &diags)
	return roomID, diags, handler.seen
}

// TestCreateRoomNotRetriedOnGatewayError is the regression test for issue #51.
//
// mautrix retries transport errors and gateway responses, and /createRoom
// carries no transaction id, so a retry after an ambiguous failure can make a
// second room that nothing ever surfaces. Counting the requests is the
// assertion: whether the call failed says nothing about how many rooms exist.
func TestCreateRoomNotRetriedOnGatewayError(t *testing.T) {
	_, diags, creates := createRoomOnce(t, http.StatusGatewayTimeout)
	if creates != 1 {
		t.Errorf("the homeserver saw %d creates, want 1: a repeat can make a second room (issue #51)", creates)
	}
	if !diags.HasError() {
		t.Fatal("want an error when the create fails")
	}
	if !strings.Contains(diags[0].Detail(), "A room may exist") {
		t.Errorf("the error must say the outcome is unknown; got %q", diags[0].Detail())
	}
}

// TestCreateRoomRetriedOnRateLimit keeps the fix from 9eb8f11 in place. A rate
// limit is refused before the homeserver does any work, so repeating it is safe,
// and Synapse limits room creation tightly enough that the acceptance suite hit
// it.
func TestCreateRoomRetriedOnRateLimit(t *testing.T) {
	roomID, diags, creates := createRoomOnce(t, http.StatusTooManyRequests)
	if diags.HasError() {
		t.Fatalf("a rate limit must be retried, not surfaced: %v", diags)
	}
	if creates != 2 {
		t.Errorf("the homeserver saw %d creates, want 2", creates)
	}
	if roomID != "!created:example.com" {
		t.Errorf("room id = %q, want the one the second attempt returned", roomID)
	}
}

// The two sentences that must reach a practitioner only when they are true.
const (
	adviceRoomMayExist = "A room may exist"
	adviceApplyAgain   = "rate limited every attempt"
	adviceInvitesSent  = "The request carried invites"
)

// TestCreateRoomAdviceMatchesTheFailure drives the real error out of
// retryOnRateLimit rather than testing the classifier alone, because the bug in
// issue #55 was in which error reaches the message, not in how it is read.
func TestCreateRoomAdviceMatchesTheFailure(t *testing.T) {
	// Every attempt refused. This is the case issue #55 reported: the
	// homeserver created nothing and applying again is the fix, yet the error
	// said a room might exist and warned against retrying.
	exhausted := make([]int, matrixHTTPRetries+1)
	for i := range exhausted {
		exhausted[i] = http.StatusTooManyRequests
	}

	cases := []struct {
		name       string
		statuses   []int
		invites    []string
		wantCreate int
		want       string
		notWant    []string
	}{
		{
			name:     "a gateway timeout leaves the outcome unknown",
			statuses: []int{http.StatusGatewayTimeout}, wantCreate: 1,
			want: adviceRoomMayExist, notWant: []string{adviceApplyAgain},
		},
		{
			name:     "an exhausted rate limit created nothing",
			statuses: exhausted, wantCreate: len(exhausted),
			want: adviceApplyAgain, notWant: []string{adviceRoomMayExist},
		},
		{
			// Synapse checks the rate limit at the top of create_room, before
			// it validates anything or touches the database, so invites in the
			// request change nothing here.
			name:     "an exhausted rate limit with invites still created nothing",
			statuses: exhausted, invites: []string{"@invitee:example.com"},
			wantCreate: len(exhausted),
			want:       adviceApplyAgain, notWant: []string{adviceRoomMayExist},
		},
		{
			// The homeserver said why. Anything this provider adds is noise.
			name:     "a refusal speaks for itself",
			statuses: []int{http.StatusForbidden}, wantCreate: 1,
			notWant: []string{adviceRoomMayExist, adviceApplyAgain},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags, creates := runCreateRoom(t,
				&createRoomServer{t: t, statuses: c.statuses}, c.invites...)
			if creates != c.wantCreate {
				t.Errorf("the homeserver saw %d creates, want %d", creates, c.wantCreate)
			}
			if !diags.HasError() {
				t.Fatal("want an error when the create fails")
			}
			detail := diags[0].Detail()
			if c.want != "" && !strings.Contains(detail, c.want) {
				t.Errorf("detail must contain %q; got %q", c.want, detail)
			}
			for _, unwanted := range c.notWant {
				if strings.Contains(detail, unwanted) {
					t.Errorf("detail must not contain %q; got %q", unwanted, detail)
				}
			}
		})
	}
}

// TestCreateRoomAdviceOnRefusedInvite covers the one refusal that still leaves a
// room behind. Synapse makes the room and its state first, then sends the
// invites one at a time and rolls nothing back if one fails, so a 403 from an
// invite arrives with the room already made. See synapse/handlers/room.py, where
// the invite loop runs after _send_events_for_new_room.
func TestCreateRoomAdviceOnRefusedInvite(t *testing.T) {
	_, diags, creates := runCreateRoom(t,
		&createRoomServer{t: t, statuses: []int{http.StatusForbidden}},
		"@invitee:example.com")
	if creates != 1 {
		t.Errorf("the homeserver saw %d creates, want 1", creates)
	}
	if !diags.HasError() {
		t.Fatal("want an error when the create fails")
	}
	if !strings.Contains(diags[0].Detail(), adviceInvitesSent) {
		t.Errorf("a refused invite can leave a room behind; got %q", diags[0].Detail())
	}
}

// TestCreateRoomAdviceOnUnreadableSuccess covers the worst outcome of all: the
// homeserver made the room and the reply never arrived intact, so nothing knows
// the room id. mautrix reports that with the 2xx response attached, so a rule
// that called every answered request a refusal would stay silent about a room
// that exists.
func TestCreateRoomAdviceOnUnreadableSuccess(t *testing.T) {
	_, diags, creates := runCreateRoom(t, &createRoomServer{
		t: t, statuses: []int{http.StatusOK}, brokenBody: true,
	})
	if creates != 1 {
		t.Errorf("the homeserver saw %d creates, want 1", creates)
	}
	if !diags.HasError() {
		t.Fatal("want an error when the reply cannot be read")
	}
	if !strings.Contains(diags[0].Detail(), adviceRoomMayExist) {
		t.Errorf("the room does exist and its id is lost; got %q", diags[0].Detail())
	}
}

// TestRateLimitWait covers what counts as a rate limit and how long to wait.
func TestRateLimitWait(t *testing.T) {
	withHeader := func(status int, code, header string) error {
		e := mautrix.HTTPError{Response: &http.Response{StatusCode: status, Header: http.Header{}}}
		if header != "" {
			e.Response.Header.Set("Retry-After", header)
		}
		if code != "" {
			e.RespError = &mautrix.RespError{ErrCode: code}
		}
		return e
	}

	cases := []struct {
		name      string
		err       error
		wantLimit bool
		want      time.Duration
	}{
		{
			name: "a Retry-After in seconds", err: withHeader(429, "M_LIMIT_EXCEEDED", "5"),
			wantLimit: true, want: 5 * time.Second,
		},
		{
			name: "recognised by status alone", err: withHeader(429, "", "1"),
			wantLimit: true, want: time.Second,
		},
		{
			name: "recognised by errcode alone", err: withHeader(400, "M_LIMIT_EXCEEDED", ""),
			wantLimit: true, want: rateLimitFallback,
		},
		{
			name: "no hint falls back", err: withHeader(429, "M_LIMIT_EXCEEDED", ""),
			wantLimit: true, want: rateLimitFallback,
		},
		{
			// A hostile or broken header must not hang an apply.
			name: "a huge wait is capped", err: withHeader(429, "M_LIMIT_EXCEEDED", "100000"),
			wantLimit: true, want: rateLimitCap,
		},
		{
			name:      "a past HTTP-date clamps to zero",
			err:       withHeader(429, "M_LIMIT_EXCEEDED", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)),
			wantLimit: true, want: 0,
		},
		{name: "a gateway error is not a rate limit", err: withHeader(504, "M_UNKNOWN", "")},
		{name: "a forbidden is not a rate limit", err: withHeader(403, "M_FORBIDDEN", "")},
		{name: "a transport error is not a rate limit", err: errors.New("connection reset")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wait, limited := rateLimitWait(c.err)
			if limited != c.wantLimit {
				t.Fatalf("limited = %v, want %v", limited, c.wantLimit)
			}
			if limited && wait != c.want {
				t.Errorf("wait = %v, want %v", wait, c.want)
			}
		})
	}
}

// TestRateLimitRetryHonoursCancellation guards the sleep between attempts, so an
// interrupted apply stops promptly instead of waiting out a Retry-After.
func TestRateLimitRetryHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	limited := mautrix.HTTPError{
		Response:  &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{}},
		RespError: &mautrix.RespError{ErrCode: "M_LIMIT_EXCEEDED"},
	}
	var calls int
	done := make(chan error, 1)
	go func() {
		_, err := retryOnRateLimit(ctx, 5, func(context.Context) (int, error) {
			calls++
			cancel()
			return 0, limited
		})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
		// The rate limit has to travel out with the cancellation, or the
		// caller reads the outcome as unknown and warns that a room may
		// exist. See issue #55.
		if ambiguousErr(err) {
			t.Errorf("a cancelled backoff created nothing; err = %v reads as ambiguous", err)
		}
	case <-time.After(rateLimitFallback + time.Second):
		t.Fatal("retryOnRateLimit ignored cancellation and waited out the backoff")
	}
	if calls != 1 {
		t.Errorf("made %d attempts after cancellation, want 1", calls)
	}
}
