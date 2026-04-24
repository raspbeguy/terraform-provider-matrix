package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/event"
)

func TestValidateJoinRulesModel_RestrictedWithoutAllow(t *testing.T) {
	m := joinRulesModel{
		JoinRule:   types.StringValue("restricted"),
		AllowRooms: types.SetNull(types.StringType),
	}
	diags := validateJoinRulesModel(m)
	if !diags.HasError() {
		t.Fatalf("expected error for restricted without allow_rooms, got %v", diags)
	}
}

func TestValidateJoinRulesModel_KnockRestrictedWithoutAllow(t *testing.T) {
	m := joinRulesModel{
		JoinRule:   types.StringValue("knock_restricted"),
		AllowRooms: types.SetNull(types.StringType),
	}
	diags := validateJoinRulesModel(m)
	if !diags.HasError() {
		t.Fatalf("expected error for knock_restricted without allow_rooms, got %v", diags)
	}
}

func TestValidateJoinRulesModel_PublicWithAllowRooms(t *testing.T) {
	allow, d := types.SetValueFrom(context.Background(), types.StringType, []string{"!space:example.com"})
	if d.HasError() {
		t.Fatalf("setting up test: %v", d)
	}
	m := joinRulesModel{
		JoinRule:   types.StringValue("public"),
		AllowRooms: allow,
	}
	diags := validateJoinRulesModel(m)
	if !diags.HasError() {
		t.Fatalf("expected error for non-restricted rule with allow_rooms, got %v", diags)
	}
}

func TestValidateJoinRulesModel_RestrictedWithAllow(t *testing.T) {
	allow, d := types.SetValueFrom(context.Background(), types.StringType, []string{"!space:example.com"})
	if d.HasError() {
		t.Fatalf("setting up test: %v", d)
	}
	m := joinRulesModel{
		JoinRule:   types.StringValue("restricted"),
		AllowRooms: allow,
	}
	diags := validateJoinRulesModel(m)
	if diags.HasError() {
		t.Fatalf("unexpected error for valid restricted+allow: %v", diags)
	}
}

func TestValidateJoinRulesModel_PublicWithoutAllow(t *testing.T) {
	m := joinRulesModel{
		JoinRule:   types.StringValue("public"),
		AllowRooms: types.SetNull(types.StringType),
	}
	diags := validateJoinRulesModel(m)
	if diags.HasError() {
		t.Fatalf("unexpected error for plain public rule: %v", diags)
	}
}

// TestBuildJoinRulesContent_TypesAllowEntries guards against the class of bug
// where each allow_rooms entry doesn't get the m.room_membership Type tag,
// which would silently break restricted-room gating.
func TestBuildJoinRulesContent_TypesAllowEntries(t *testing.T) {
	ctx := context.Background()
	allow, d := types.SetValueFrom(ctx, types.StringType, []string{
		"!space-one:example.com",
		"!space-two:example.com",
	})
	if d.HasError() {
		t.Fatalf("setting up test: %v", d)
	}
	m := &joinRulesModel{
		JoinRule:   types.StringValue("restricted"),
		AllowRooms: allow,
	}

	got, err := buildJoinRulesContent(ctx, m)
	if err != nil {
		t.Fatalf("buildJoinRulesContent: %v", err)
	}
	if got.JoinRule != event.JoinRuleRestricted {
		t.Errorf("JoinRule = %q, want %q", got.JoinRule, event.JoinRuleRestricted)
	}
	if len(got.Allow) != 2 {
		t.Fatalf("Allow len = %d, want 2", len(got.Allow))
	}
	seen := map[string]bool{}
	for i, a := range got.Allow {
		if a.Type != event.JoinRuleAllowRoomMembership {
			t.Errorf("Allow[%d].Type = %q, want %q", i, a.Type, event.JoinRuleAllowRoomMembership)
		}
		seen[string(a.RoomID)] = true
	}
	for _, expected := range []string{"!space-one:example.com", "!space-two:example.com"} {
		if !seen[expected] {
			t.Errorf("missing allow entry for %q", expected)
		}
	}
}

func TestBuildJoinRulesContent_NoAllowRooms(t *testing.T) {
	ctx := context.Background()
	m := &joinRulesModel{
		JoinRule:   types.StringValue("public"),
		AllowRooms: types.SetNull(types.StringType),
	}
	got, err := buildJoinRulesContent(ctx, m)
	if err != nil {
		t.Fatalf("buildJoinRulesContent: %v", err)
	}
	if got.JoinRule != event.JoinRulePublic {
		t.Errorf("JoinRule = %q, want %q", got.JoinRule, event.JoinRulePublic)
	}
	if len(got.Allow) != 0 {
		t.Errorf("Allow should be empty for non-restricted rule; got %d entries", len(got.Allow))
	}
}
