package provider

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

// testAccProtoV6ProviderFactories wires the provider into the test harness
// in-process, so no build, install or registry lookup is involved.
//
// The harness reattaches this server under an address built from
// TF_ACC_PROVIDER_HOST and TF_ACC_PROVIDER_NAMESPACE, which default to
// registry.terraform.io/hashicorp. The test configs declare
// source = "raspbeguy/matrix", so a run against OpenTofu needs both variables
// set; .github/workflows/acceptance.yml does that. Without them OpenTofu
// rejects the default legacy "-" namespace outright.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"matrix": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccPreCheck fails fast with a readable message when the acceptance
// environment is missing, rather than surfacing a /whoami error mid-plan.
// .github/workflows/acceptance.yml sets these against a Synapse container.
func testAccPreCheck(t *testing.T) {
	t.Helper()
	for _, key := range []string{"MATRIX_HOMESERVER_URL", "MATRIX_ACCESS_TOKEN"} {
		if os.Getenv(key) == "" {
			t.Fatalf("%s must be set for TestAcc tests", key)
		}
	}
}

// testAccSkipUnlessAcc skips before any network call. resource.Test makes the
// same TF_ACC check itself, but a test that needs the caller's mxid to build
// its assertions has to skip earlier than that.
func testAccSkipUnlessAcc(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set; skipping acceptance test")
	}
}

var (
	testAccClientOnce sync.Once
	testAccClientVal  *Client
	testAccClientErr  error
)

var (
	testAccPublishOnce sync.Once
	testAccPublishes   bool
)

// testAccHomeserverPublishes reports whether this homeserver will list a room in
// the public directory.
//
// Synapse denies it by default, through room_list_publication_rules, so the
// answer differs between the CI container, which #44 configures to allow it, and
// a typical deployment. A test that assumed one of them is how issue #49
// happened.
//
// It has to be probed rather than tolerated. On a Config step
// ExpectNonEmptyPlan is strict in both directions: an unexpected non-empty plan
// fails, and so does an expected one that turns out empty. There is no setting
// that accepts either, and the steps are declared before anything runs.
//
// Probed once per run, with a throwaway room. That room is left behind like
// every other room these tests create, because Matrix has no way to delete one.
func testAccHomeserverPublishes(t *testing.T) bool {
	t.Helper()
	testAccPublishOnce.Do(func() {
		c := testAccClient(t)
		resp, err := c.MX.CreateRoom(context.Background(), &mautrix.ReqCreateRoom{
			Name:       "tf-acc-publication-probe",
			Preset:     "private_chat",
			Visibility: "public",
		})
		if err != nil {
			t.Fatalf("probe room: %v", err)
		}
		vis, found, err := getRoomVisibility(context.Background(), c, resp.RoomID)
		testAccPublishes = err == nil && found && vis == "public"
	})
	return testAccPublishes
}

// testAccClient returns a raw client so a test can assert on what actually
// landed on the homeserver, not only on what the provider wrote to state.
//
// Built once. Every check function used to build its own and call /whoami,
// which spends the single test account's rate-limit budget on nothing. It also
// sets the same retry count the provider does: mautrix.NewClient leaves
// DefaultHTTPRetries at zero, so without it a helper gives up on the first 429,
// which is how the suite failed once it grew past five tests.
func testAccClient(t *testing.T) *Client {
	t.Helper()
	testAccClientOnce.Do(func() {
		mcli, err := mautrix.NewClient(os.Getenv("MATRIX_HOMESERVER_URL"), id.UserID(os.Getenv("MATRIX_USER_ID")), os.Getenv("MATRIX_ACCESS_TOKEN"))
		if err != nil {
			testAccClientErr = fmt.Errorf("mautrix.NewClient: %w", err)
			return
		}
		mcli.DefaultHTTPRetries = matrixHTTPRetries
		who, err := mcli.Whoami(context.Background())
		if err != nil {
			testAccClientErr = fmt.Errorf("whoami: %w", err)
			return
		}
		mcli.UserID = who.UserID
		testAccClientVal = &Client{MX: mcli}
	})
	if testAccClientErr != nil {
		t.Fatalf("%v", testAccClientErr)
	}
	return testAccClientVal
}

// testAccUserID is the mxid the run authenticates as. The CI homeserver
// registers exactly one account, so tests never assume a second party exists.
func testAccUserID(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("MATRIX_USER_ID"); v != "" {
		return v
	}
	return string(testAccClient(t).MX.UserID)
}

// TestMatrixHTTPRetries guards that the provider retries rate limits at all.
// mautrix retries a 429 and honours Retry-After, but only when
// DefaultHTTPRetries is above zero, and mautrix.NewClient leaves it at zero.
// With it at zero a configuration that creates several rooms fails part way
// through with M_LIMIT_EXCEEDED, which is how the acceptance suite found this.
func TestMatrixHTTPRetries(t *testing.T) {
	if matrixHTTPRetries <= 0 {
		t.Fatalf("matrixHTTPRetries = %d; a homeserver rate limit must be retried", matrixHTTPRetries)
	}
	mcli, err := mautrix.NewClient("https://example.com", "@bot:example.com", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if mcli.DefaultHTTPRetries != 0 {
		t.Skip("mautrix now retries by default; the provider no longer needs to set it")
	}
	if mcli.IgnoreRateLimit {
		t.Error("IgnoreRateLimit must stay false, or a 429 is never retried")
	}
}
