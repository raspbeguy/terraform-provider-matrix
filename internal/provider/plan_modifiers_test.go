package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestJSONSemanticEquality_SuppressesReorderedKeys(t *testing.T) {
	prior := types.StringValue(`{"a":1,"b":2}`)
	planned := types.StringValue(`{"b":2,"a":1}`)

	resp := &planmodifier.StringResponse{PlanValue: planned}
	jsonSemanticEqualityModifier{}.PlanModifyString(context.Background(), planmodifier.StringRequest{
		StateValue: prior,
		PlanValue:  planned,
	}, resp)

	if resp.PlanValue.ValueString() != prior.ValueString() {
		t.Fatalf("expected plan to be collapsed to prior state (%q); got %q",
			prior.ValueString(), resp.PlanValue.ValueString())
	}
}

func TestJSONSemanticEquality_RealDrift(t *testing.T) {
	prior := types.StringValue(`{"a":1}`)
	planned := types.StringValue(`{"a":2}`)

	resp := &planmodifier.StringResponse{PlanValue: planned}
	jsonSemanticEqualityModifier{}.PlanModifyString(context.Background(), planmodifier.StringRequest{
		StateValue: prior,
		PlanValue:  planned,
	}, resp)

	if resp.PlanValue.ValueString() != planned.ValueString() {
		t.Fatalf("expected plan to remain unchanged on real drift; got %q",
			resp.PlanValue.ValueString())
	}
}
