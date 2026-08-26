package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
		{name: "a 404 means it is already gone", err: httpErr(http.StatusNotFound, "")},
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

// createRoomOnce drives createRoomLike against a scripted server and reports how
// many creates arrived.
func createRoomOnce(t *testing.T, statuses ...int) (id.RoomID, diag.Diagnostics, int) {
	t.Helper()
	handler := &createRoomServer{t: t, statuses: statuses}
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
		InitialInvites:    types.SetNull(types.StringType),
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
	case <-time.After(rateLimitFallback + time.Second):
		t.Fatal("retryOnRateLimit ignored cancellation and waited out the backoff")
	}
	if calls != 1 {
		t.Errorf("made %d attempts after cancellation, want 1", calls)
	}
}
