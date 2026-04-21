package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
)

// oneOfString is a tiny string validator that accepts any value from the list.
type oneOfString []string

func (v oneOfString) Description(_ context.Context) string {
	return "must be one of: " + strings.Join(v, ", ")
}
func (v oneOfString) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (v oneOfString) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	value := req.ConfigValue.ValueString()
	for _, allowed := range v {
		if value == allowed {
			return
		}
	}
	resp.Diagnostics.AddAttributeError(req.Path, "Invalid value",
		"got "+value+"; "+v.Description(context.Background()))
}

func errorFromDiags(d diag.Diagnostics) error {
	if !d.HasError() {
		return nil
	}
	return diagsError(d)
}

type diagsError diag.Diagnostics

func (d diagsError) Error() string {
	msgs := make([]string, 0, len(d))
	for _, e := range d {
		msgs = append(msgs, e.Summary()+": "+e.Detail())
	}
	return strings.Join(msgs, "; ")
}
