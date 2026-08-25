package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TestMembershipSatisfiedBy guards the satisfaction table that drives both
// applyMembership's idempotency check and the plan-time drift suppression.
// Any divergence between these two rule sets reintroduces the perpetual-drift
// bug fixed in v0.3.1.
func TestMembershipSatisfiedBy(t *testing.T) {
	cases := []struct {
		wanted, current string
		want            bool
	}{
		// invite is satisfied by invite or join (already in or being invited).
		{"invite", "invite", true},
		{"invite", "join", true},
		{"invite", "leave", false},
		{"invite", "ban", false},
		{"invite", "knock", false},

		// knock is satisfied by knock, invite, or join.
		{"knock", "knock", true},
		{"knock", "invite", true},
		{"knock", "join", true},
		{"knock", "leave", false},
		{"knock", "ban", false},

		// leave is satisfied by leave or ban (banned is "more left than left").
		//
		// Destroy relies on this. applyMembership returns nil for both, so the
		// already-gone case never reaches an error, which is why Delete can treat
		// every error as a real refusal rather than swallowing it. See issue #45.
		{"leave", "leave", true},
		{"leave", "ban", true},
		{"leave", "invite", false},
		{"leave", "join", false},
		{"leave", "knock", false},

		// ban only satisfies itself.
		{"ban", "ban", true},
		{"ban", "leave", false},
		{"ban", "invite", false},
		{"ban", "join", false},

		// join only satisfies itself; can't promote leave/invite into join.
		{"join", "join", true},
		{"join", "invite", false},
		{"join", "leave", false},
		{"join", "ban", false},

		// Unknown wanted values never satisfy.
		{"weird", "join", false},
		{"", "join", false},
	}
	for _, c := range cases {
		got := membershipSatisfiedBy(c.wanted, c.current)
		if got != c.want {
			t.Errorf("membershipSatisfiedBy(%q, %q) = %v, want %v", c.wanted, c.current, got, c.want)
		}
	}
}

// TestMembershipSatisfiedByModifier_NullGuards covers the IsNull/IsUnknown
// short-circuits in PlanModifyString. Without these, a Create with no prior
// state, or a plan with a not-yet-resolved reference, would panic.
func TestMembershipSatisfiedByModifier_NullGuards(t *testing.T) {
	cases := []struct {
		name             string
		state, plan      types.String
		wantPlanValueStr string // expected plan value after the modifier runs
	}{
		{"state null (Create)", types.StringNull(), types.StringValue("invite"), "invite"},
		{"plan null", types.StringValue("join"), types.StringNull(), ""},
		{"plan unknown", types.StringValue("join"), types.StringUnknown(), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &planmodifier.StringResponse{PlanValue: c.plan}
			membershipSatisfiedByModifier{}.PlanModifyString(context.Background(),
				planmodifier.StringRequest{StateValue: c.state, PlanValue: c.plan}, resp)
			got := ""
			if !resp.PlanValue.IsNull() && !resp.PlanValue.IsUnknown() {
				got = resp.PlanValue.ValueString()
			}
			if got != c.wantPlanValueStr {
				t.Errorf("plan value = %q, want %q", got, c.wantPlanValueStr)
			}
		})
	}
}

// TestMembershipSatisfiedByModifier_SuppressesSatisfiedDrift confirms the
// happy path: HCL "invite" against state "join" should land at planned "join".
func TestMembershipSatisfiedByModifier_SuppressesSatisfiedDrift(t *testing.T) {
	resp := &planmodifier.StringResponse{PlanValue: types.StringValue("invite")}
	membershipSatisfiedByModifier{}.PlanModifyString(context.Background(),
		planmodifier.StringRequest{
			StateValue: types.StringValue("join"),
			PlanValue:  types.StringValue("invite"),
		}, resp)
	if resp.PlanValue.ValueString() != "join" {
		t.Errorf("expected plan value to collapse to state (\"join\"); got %q", resp.PlanValue.ValueString())
	}
}

// TestMembershipSatisfiedByModifier_PassesRealDrift confirms a genuine
// transition (e.g. leave -> invite) is not suppressed.
func TestMembershipSatisfiedByModifier_PassesRealDrift(t *testing.T) {
	resp := &planmodifier.StringResponse{PlanValue: types.StringValue("invite")}
	membershipSatisfiedByModifier{}.PlanModifyString(context.Background(),
		planmodifier.StringRequest{
			StateValue: types.StringValue("leave"),
			PlanValue:  types.StringValue("invite"),
		}, resp)
	if resp.PlanValue.ValueString() != "invite" {
		t.Errorf("expected plan value unchanged on real drift; got %q", resp.PlanValue.ValueString())
	}
}
