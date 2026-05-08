package provider

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
)

// membershipSatisfiedByModifier suppresses plan-time drift when the current
// (state) membership semantically satisfies the desired (HCL) one. Without it,
// HCL declaring `membership = "invite"` against a server-side `"join"` would
// show a perpetual `join -> invite` diff even though apply would no-op.
//
// The same satisfaction table is used by applyMembership at apply time, so
// plan and apply stay consistent.
type membershipSatisfiedByModifier struct{}

func (membershipSatisfiedByModifier) Description(_ context.Context) string {
	return "Suppresses planned change when the current membership already satisfies the desired one (e.g. \"join\" satisfies \"invite\")."
}
func (m membershipSatisfiedByModifier) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (membershipSatisfiedByModifier) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	if membershipSatisfiedBy(req.PlanValue.ValueString(), req.StateValue.ValueString()) {
		resp.PlanValue = req.StateValue
	}
}

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
