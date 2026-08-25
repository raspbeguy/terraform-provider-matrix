package provider

import (
	"context"
	"os"
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

// testAccClient builds a raw client so a test can assert on what actually
// landed on the homeserver, not only on what the provider wrote to state.
func testAccClient(t *testing.T) *Client {
	t.Helper()
	mcli, err := mautrix.NewClient(os.Getenv("MATRIX_HOMESERVER_URL"), id.UserID(os.Getenv("MATRIX_USER_ID")), os.Getenv("MATRIX_ACCESS_TOKEN"))
	if err != nil {
		t.Fatalf("mautrix.NewClient: %v", err)
	}
	who, err := mcli.Whoami(context.Background())
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	mcli.UserID = who.UserID
	return &Client{MX: mcli}
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
