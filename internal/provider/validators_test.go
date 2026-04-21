package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestOneOfString(t *testing.T) {
	v := oneOfString{"invite", "join", "leave", "ban", "knock"}

	for _, ok := range []string{"invite", "join", "leave", "ban", "knock"} {
		resp := &validator.StringResponse{}
		v.ValidateString(context.Background(), validator.StringRequest{
			Path:        path.Root("membership"),
			ConfigValue: types.StringValue(ok),
		}, resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("expected %q to be accepted, got %v", ok, resp.Diagnostics)
		}
	}

	resp := &validator.StringResponse{}
	v.ValidateString(context.Background(), validator.StringRequest{
		Path:        path.Root("membership"),
		ConfigValue: types.StringValue("bogus"),
	}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("expected bogus value to fail validation")
	}
}
