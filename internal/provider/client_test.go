package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"

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
