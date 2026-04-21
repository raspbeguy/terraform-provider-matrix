package provider

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
)

// jsonSemanticEqualityModifier suppresses diff when the prior and planned values are
// semantically equal JSON (same keys/values, different whitespace or key order).
type jsonSemanticEqualityModifier struct{}

func (jsonSemanticEqualityModifier) Description(_ context.Context) string {
	return "Suppresses drift when the old and new JSON values are semantically equal."
}
func (m jsonSemanticEqualityModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (jsonSemanticEqualityModifier) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	var a, b any
	if err := json.Unmarshal([]byte(req.StateValue.ValueString()), &a); err != nil {
		return
	}
	if err := json.Unmarshal([]byte(req.PlanValue.ValueString()), &b); err != nil {
		return
	}
	if reflect.DeepEqual(a, b) {
		resp.PlanValue = req.StateValue
	}
}
