package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// powerLevelsKnobs is every Optional+Computed attribute of the resource. The
// whole design of this resource rests on that shape: Create resolves the ones
// the plan left unknown, and UseStateForUnknown reuses the prior value on
// later plans. Dropping Computed from any of them reintroduces issue #29.
var powerLevelsKnobs = []string{
	"users_default", "events_default", "state_default", "ban", "kick",
	"invite", "redact", "users", "events", "notify_room",
}

func mustMap(t *testing.T, raw map[string]int64) types.Map {
	t.Helper()
	v, d := types.MapValueFrom(context.Background(), types.Int64Type, raw)
	if d.HasError() {
		t.Fatalf("MapValueFrom(%v): %v", raw, d)
	}
	return v
}

// nothingDeclared is a model in which the practitioner declared no knob at all:
// half null (not managed), half unknown (Computed, unresolved during Create).
func nothingDeclared() *powerLevelsModel {
	return &powerLevelsModel{
		UsersDefault:  types.Int64Unknown(),
		EventsDefault: types.Int64Null(),
		StateDefault:  types.Int64Unknown(),
		Ban:           types.Int64Null(),
		Kick:          types.Int64Unknown(),
		Invite:        types.Int64Null(),
		Redact:        types.Int64Unknown(),
		Users:         types.MapUnknown(types.Int64Type),
		Events:        types.MapNull(types.Int64Type),
		NotifyRoom:    types.Int64Unknown(),
	}
}

// populatedContent is a room whose power levels are far from the spec defaults,
// the way a real room or a space created by createRoomLike looks.
func populatedContent() *event.PowerLevelsEventContent {
	invite, kick, ban, redact, state, notify := 50, 60, 70, 80, 90, 95
	return &event.PowerLevelsEventContent{
		Users:           map[id.UserID]int{"@bot:example.com": 100},
		UsersDefault:    5,
		Events:          map[string]int{"m.room.name": 50},
		EventsDefault:   100,
		InvitePtr:       &invite,
		KickPtr:         &kick,
		BanPtr:          &ban,
		RedactPtr:       &redact,
		StateDefaultPtr: &state,
		Notifications:   &event.NotificationPowerLevels{RoomPtr: &notify},
	}
}

// TestApplyModelToPowerLevels_PreservesUndeclaredFields is the regression test
// for the second defect in issue #29. m.room.power_levels is replaced wholesale
// by the PUT, so a partial config used to wipe every value it did not mention,
// including the creating account's own users entry (level 100 -> users_default
// 0, which is below state_default 50, so the room became unmanageable) and the
// space defaults set in createRoomLike (events_default 100, invite 50).
func TestApplyModelToPowerLevels_PreservesUndeclaredFields(t *testing.T) {
	pl := populatedContent()
	m := nothingDeclared()
	m.UsersDefault = types.Int64Value(20) // the one field the practitioner declared

	if err := applyModelToPowerLevels(context.Background(), pl, m); err != nil {
		t.Fatalf("applyModelToPowerLevels: %v", err)
	}

	if got := pl.Users["@bot:example.com"]; got != 100 {
		t.Errorf("caller's users entry: got %d, want 100 (self-lockout, issue #29)", got)
	}
	if pl.EventsDefault != 100 {
		t.Errorf("events_default: got %d, want 100 (space default)", pl.EventsDefault)
	}
	if pl.InvitePtr == nil || *pl.InvitePtr != 50 {
		t.Errorf("invite: got %v, want 50 (space default)", pl.InvitePtr)
	}
	if got := pl.Events["m.room.name"]; got != 50 {
		t.Errorf("events entry: got %d, want 50", got)
	}
	if pl.UsersDefault != 20 {
		t.Errorf("users_default: got %d, want the declared 20", pl.UsersDefault)
	}
}

// TestApplyModelToPowerLevels_SkipsNullAndUnknown checks the overlay writes
// nothing at all when the practitioner declared nothing.
func TestApplyModelToPowerLevels_SkipsNullAndUnknown(t *testing.T) {
	pl := populatedContent()
	if err := applyModelToPowerLevels(context.Background(), pl, nothingDeclared()); err != nil {
		t.Fatalf("applyModelToPowerLevels: %v", err)
	}

	want := populatedContent()
	if pl.UsersDefault != want.UsersDefault || pl.EventsDefault != want.EventsDefault {
		t.Errorf("defaults changed: got %d/%d", pl.UsersDefault, pl.EventsDefault)
	}
	for name, pair := range map[string][2]*int{
		"invite":        {pl.InvitePtr, want.InvitePtr},
		"kick":          {pl.KickPtr, want.KickPtr},
		"ban":           {pl.BanPtr, want.BanPtr},
		"redact":        {pl.RedactPtr, want.RedactPtr},
		"state_default": {pl.StateDefaultPtr, want.StateDefaultPtr},
	} {
		if pair[0] == nil || pair[1] == nil || *pair[0] != *pair[1] {
			t.Errorf("%s changed: got %v, want %v", name, pair[0], pair[1])
		}
	}
	if len(pl.Users) != 1 || len(pl.Events) != 1 {
		t.Errorf("maps changed: users=%v events=%v", pl.Users, pl.Events)
	}
	if pl.Notifications == nil || pl.Notifications.RoomPtr == nil || *pl.Notifications.RoomPtr != 95 {
		t.Errorf("notifications changed: %v", pl.Notifications)
	}
}

// TestApplyModelToPowerLevels_ZeroIsDeclaredNotSkipped is why the overlay reads
// the types.Int64 rather than merging two content structs: UsersDefault and
// EventsDefault are plain ints, so a declared 0 and an undeclared field are
// indistinguishable once mapped into event.PowerLevelsEventContent.
func TestApplyModelToPowerLevels_ZeroIsDeclaredNotSkipped(t *testing.T) {
	pl := populatedContent()
	m := nothingDeclared()
	m.UsersDefault = types.Int64Value(0)
	m.EventsDefault = types.Int64Value(0)

	if err := applyModelToPowerLevels(context.Background(), pl, m); err != nil {
		t.Fatalf("applyModelToPowerLevels: %v", err)
	}
	if pl.UsersDefault != 0 {
		t.Errorf("users_default: got %d, want the declared 0", pl.UsersDefault)
	}
	if pl.EventsDefault != 0 {
		t.Errorf("events_default: got %d, want the declared 0", pl.EventsDefault)
	}
}

// TestApplyModelToPowerLevels_DeclaredUsersReplacesMap locks in that a declared
// map is authoritative. Merging per key instead would make it impossible to
// remove a user's level — and it is precisely this wholesale replacement that
// powerLevelsSelfLockoutWarnings warns about.
func TestApplyModelToPowerLevels_DeclaredUsersReplacesMap(t *testing.T) {
	pl := populatedContent()
	pl.Users["@old:example.com"] = 50
	m := nothingDeclared()
	m.Users = mustMap(t, map[string]int64{"@alice:example.com": 100})

	if err := applyModelToPowerLevels(context.Background(), pl, m); err != nil {
		t.Fatalf("applyModelToPowerLevels: %v", err)
	}
	if len(pl.Users) != 1 || pl.Users["@alice:example.com"] != 100 {
		t.Errorf("declared users must replace the map wholesale, got %v", pl.Users)
	}
}

// TestApplyModelToPowerLevels_DeclaredEmptyMapClears checks that `users = {}`
// clears the map rather than being treated as undeclared.
func TestApplyModelToPowerLevels_DeclaredEmptyMapClears(t *testing.T) {
	pl := populatedContent()
	m := nothingDeclared()
	m.Users = mustMap(t, map[string]int64{})

	if err := applyModelToPowerLevels(context.Background(), pl, m); err != nil {
		t.Fatalf("applyModelToPowerLevels: %v", err)
	}
	if pl.Users == nil || len(pl.Users) != 0 {
		t.Errorf("users = {} must clear the map, got %v", pl.Users)
	}
}

// TestResolveUnknownPowerLevels_FillsEveryUnknown is the regression test for the
// first defect in issue #29. Create wrote the plan straight to state, and at
// create time UseStateForUnknown bails (there is no prior state), so every
// undeclared knob was still unknown. Terraform then rejected the apply with
// "Provider returned invalid result object after apply".
func TestResolveUnknownPowerLevels_FillsEveryUnknown(t *testing.T) {
	m := &powerLevelsModel{
		UsersDefault:  types.Int64Unknown(),
		EventsDefault: types.Int64Unknown(),
		StateDefault:  types.Int64Unknown(),
		Ban:           types.Int64Unknown(),
		Kick:          types.Int64Unknown(),
		Invite:        types.Int64Unknown(),
		Redact:        types.Int64Unknown(),
		Users:         types.MapUnknown(types.Int64Type),
		Events:        types.MapUnknown(types.Int64Type),
		NotifyRoom:    types.Int64Unknown(),
	}
	if err := resolveUnknownPowerLevels(context.Background(), populatedContent(), m); err != nil {
		t.Fatalf("resolveUnknownPowerLevels: %v", err)
	}

	for name, v := range map[string]interface{ IsUnknown() bool }{
		"users_default": m.UsersDefault, "events_default": m.EventsDefault,
		"state_default": m.StateDefault, "ban": m.Ban, "kick": m.Kick,
		"invite": m.Invite, "redact": m.Redact, "users": m.Users,
		"events": m.Events, "notify_room": m.NotifyRoom,
	} {
		if v.IsUnknown() {
			t.Errorf("%s is still unknown after apply (issue #29)", name)
		}
	}
	if m.UsersDefault.ValueInt64() != 5 || m.Ban.ValueInt64() != 70 || m.NotifyRoom.ValueInt64() != 95 {
		t.Errorf("resolved wrong values: %v", m)
	}
}

// TestResolveUnknownPowerLevels_KeepsKnownPlanValues guards the rule that makes
// the resolution legal: Terraform rejects an applied value that differs from a
// known planned value, and a planned null is a known value.
func TestResolveUnknownPowerLevels_KeepsKnownPlanValues(t *testing.T) {
	m := nothingDeclared()
	m.UsersDefault = types.Int64Value(7)
	m.NotifyRoom = types.Int64Null()
	m.Events = types.MapNull(types.Int64Type)

	if err := resolveUnknownPowerLevels(context.Background(), populatedContent(), m); err != nil {
		t.Fatalf("resolveUnknownPowerLevels: %v", err)
	}
	if m.UsersDefault.ValueInt64() != 7 {
		t.Errorf("known plan value overwritten: got %v, want 7", m.UsersDefault)
	}
	if !m.NotifyRoom.IsNull() {
		t.Errorf("planned null overwritten: got %v", m.NotifyRoom)
	}
	if !m.Events.IsNull() {
		t.Errorf("planned null map overwritten: got %v", m.Events)
	}
}

// TestResolveUnknownPowerLevels_DoesNotTouchIDs — Create and Update set these
// themselves, and modelFromPowerLevels has never owned them.
func TestResolveUnknownPowerLevels_DoesNotTouchIDs(t *testing.T) {
	m := nothingDeclared()
	m.ID = types.StringValue("!room:example.com")
	m.RoomID = types.StringValue("!room:example.com")

	if err := resolveUnknownPowerLevels(context.Background(), populatedContent(), m); err != nil {
		t.Fatalf("resolveUnknownPowerLevels: %v", err)
	}
	if m.ID.ValueString() != "!room:example.com" || m.RoomID.ValueString() != "!room:example.com" {
		t.Errorf("ids changed: id=%v room_id=%v", m.ID, m.RoomID)
	}
}

// TestModelFromPowerLevels_AbsentPointersBecomeNull documents the asymmetry in
// event.PowerLevelsEventContent: the *int fields report "not set" as nil, while
// users_default and events_default are plain ints whose absence reads back as 0.
func TestModelFromPowerLevels_AbsentPointersBecomeNull(t *testing.T) {
	var m powerLevelsModel
	if err := modelFromPowerLevels(context.Background(), &event.PowerLevelsEventContent{}, &m); err != nil {
		t.Fatalf("modelFromPowerLevels: %v", err)
	}
	for name, v := range map[string]interface{ IsNull() bool }{
		"state_default": m.StateDefault, "ban": m.Ban, "kick": m.Kick,
		"invite": m.Invite, "redact": m.Redact, "notify_room": m.NotifyRoom,
		"users": m.Users, "events": m.Events,
	} {
		if !v.IsNull() {
			t.Errorf("%s: got %v, want null", name, v)
		}
	}
	if m.UsersDefault.ValueInt64() != 0 || m.EventsDefault.ValueInt64() != 0 {
		t.Errorf("defaults: got %v/%v, want 0/0", m.UsersDefault, m.EventsDefault)
	}
}

// TestModelFromPowerLevels_PreservesDeclaredEmptyMaps guards a perpetual-diff
// bug. `users = {}` is dropped from the PUT by omitempty, so the read-back sees
// no key. Flipping the declared empty map to null would show a diff on every
// plan, forever.
func TestModelFromPowerLevels_PreservesDeclaredEmptyMaps(t *testing.T) {
	m := &powerLevelsModel{
		Users:  mustMap(t, map[string]int64{}),
		Events: types.MapNull(types.Int64Type),
	}
	if err := modelFromPowerLevels(context.Background(), &event.PowerLevelsEventContent{}, m); err != nil {
		t.Fatalf("modelFromPowerLevels: %v", err)
	}
	if m.Users.IsNull() || len(m.Users.Elements()) != 0 {
		t.Errorf("declared empty users map: got %v, want a known empty map", m.Users)
	}
	if !m.Events.IsNull() {
		t.Errorf("undeclared events map: got %v, want null", m.Events)
	}
}

// TestModelFromPowerLevels_PopulatesMaps is the ordinary refresh path.
func TestModelFromPowerLevels_PopulatesMaps(t *testing.T) {
	var m powerLevelsModel
	if err := modelFromPowerLevels(context.Background(), populatedContent(), &m); err != nil {
		t.Fatalf("modelFromPowerLevels: %v", err)
	}
	users := map[string]int64{}
	if d := m.Users.ElementsAs(context.Background(), &users, false); d.HasError() {
		t.Fatalf("ElementsAs: %v", d)
	}
	if users["@bot:example.com"] != 100 {
		t.Errorf("users: got %v, want @bot:example.com at 100", users)
	}
}

// TestPowerLevelsSelfLockoutWarnings covers the plan-time guard for the
// irreversible half of issue #29, and the users_default hole reported as #33.
//
// The Matrix auth rules bound what can happen here, and they are the reason the
// table looks the way it does. A sender may not set any power value above its
// own level, so a plan that only raises state_default, or only raises
// events["m.room.power_levels"], is rejected by the homeserver rather than
// applied. A raise alone can therefore never lock the caller out.
//
// Every reachable silent lockout is a drop in the caller's own level:
//
//   - a declared users map that omits or demotes the caller, which is #29
//   - a users_default drop when the caller has no entry of its own, which is #33
//
// Self-demotion is allowed by the auth rules, which is exactly why these are
// silent and irreversible.
func TestPowerLevelsSelfLockoutWarnings(t *testing.T) {
	const caller = "@bot:example.com"
	// What the room holds today: the caller at the given level, no state_default,
	// so the spec default of 50 is the bar.
	callerAt := func(level int64) *powerLevelsModel {
		return &powerLevelsModel{Users: mustMap(t, map[string]int64{caller: level})}
	}

	cases := []struct {
		name    string
		caller  string
		plan    *powerLevelsModel
		prior   *powerLevelsModel
		want    int
		wantSub string
	}{
		{
			name: "users undeclared and nothing changes", caller: caller, want: 0,
			plan:  &powerLevelsModel{Users: types.MapNull(types.Int64Type)},
			prior: callerAt(50),
		},
		{
			name: "users unknown and nothing changes", caller: caller, want: 0,
			plan:  &powerLevelsModel{Users: types.MapUnknown(types.Int64Type)},
			prior: callerAt(50),
		},
		{
			name: "caller listed at 100", caller: caller, want: 0,
			plan: &powerLevelsModel{Users: mustMap(t, map[string]int64{caller: 100})},
		},
		{
			name: "others only, falls back to users_default 0", caller: caller, want: 1,
			wantSub: "does not list your own account",
			plan:    &powerLevelsModel{Users: mustMap(t, map[string]int64{"@alice:example.com": 100})},
		},
		{
			name: "others only, but users_default is high enough", caller: caller, want: 0,
			plan: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{"@alice:example.com": 100}),
				UsersDefault: types.Int64Value(100),
			},
		},
		{
			name: "caller listed below state_default", caller: caller, want: 1,
			wantSub: "puts your own account",
			plan: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{caller: 10}),
				StateDefault: types.Int64Value(50),
			},
		},
		{
			name: "others only, but state_default is 0", caller: caller, want: 0,
			plan: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{"@alice:example.com": 100}),
				StateDefault: types.Int64Value(0),
			},
		},
		{
			name: "events override raises the bar out of a declared reach", caller: caller, want: 1,
			wantSub: "puts your own account",
			plan: &powerLevelsModel{
				Users:  mustMap(t, map[string]int64{caller: 50}),
				Events: mustMap(t, map[string]int64{"m.room.power_levels": 100}),
			},
		},
		{
			name: "declared empty users map", caller: caller, want: 1,
			wantSub: "does not list your own account",
			plan:    &powerLevelsModel{Users: mustMap(t, map[string]int64{})},
		},
		{
			// Issue #33, the reachable case: the caller has no entry of its own,
			// so users_default carries it, and the plan drops users_default
			// below the bar. The auth rules allow it, because neither the old
			// nor the new value is above the sender's level.
			name: "users_default drops the caller below the bar", caller: caller, want: 1,
			wantSub: "`users_default` leaves your own account",
			plan: &powerLevelsModel{
				Users:        types.MapNull(types.Int64Type),
				UsersDefault: types.Int64Value(0),
			},
			prior: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{"@alice:example.com": 100}),
				UsersDefault: types.Int64Value(50),
			},
		},
		{
			name: "users_default drops but stays above the bar", caller: caller, want: 0,
			plan: &powerLevelsModel{
				Users:        types.MapNull(types.Int64Type),
				UsersDefault: types.Int64Value(50),
			},
			prior: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{"@alice:example.com": 100}),
				UsersDefault: types.Int64Value(100),
			},
		},
		{
			// A raise alone. The homeserver refuses any new value above the
			// sender's own level, so this apply is rejected, not applied. It is
			// not a lockout and must not be reported as one.
			name: "state_default raised above the caller is rejected, not a lockout", caller: caller, want: 0,
			plan: &powerLevelsModel{
				Users:        types.MapNull(types.Int64Type),
				StateDefault: types.Int64Value(100),
			},
			prior: callerAt(50),
		},
		{
			name: "events entry raised above the caller is rejected, not a lockout", caller: caller, want: 0,
			plan: &powerLevelsModel{
				Users:  types.MapNull(types.Int64Type),
				Events: mustMap(t, map[string]int64{"m.room.power_levels": 100}),
			},
			prior: callerAt(50),
		},
		{
			name: "state_default raised but the caller clears it", caller: caller, want: 0,
			plan: &powerLevelsModel{
				Users:        types.MapNull(types.Int64Type),
				StateDefault: types.Int64Value(100),
			},
			prior: callerAt(100),
		},
		{
			// The caller already could not send. This plan is not what locked it
			// out, and the homeserver refuses the send with a plain error.
			name: "caller already below the bar", caller: caller, want: 0,
			plan: &powerLevelsModel{Users: types.MapNull(types.Int64Type)},
			prior: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{caller: 50}),
				StateDefault: types.Int64Value(100),
			},
		},
		{
			// Already below the bar, and the plan demotes further. Still not this
			// plan's doing: the caller could not send before it either.
			name: "caller already below the bar, and demoted further", caller: caller, want: 0,
			plan: &powerLevelsModel{Users: mustMap(t, map[string]int64{})},
			prior: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{caller: 50}),
				StateDefault: types.Int64Value(100),
			},
		},
		{
			// A create against a room the provider cannot read: no prior, so the
			// caller's level is unknowable when users is undeclared.
			name: "no prior state", caller: caller, want: 0,
			plan: &powerLevelsModel{
				Users:        types.MapNull(types.Int64Type),
				StateDefault: types.Int64Value(100),
			},
		},
		{
			name: "no caller known", caller: "", want: 0,
			plan: &powerLevelsModel{Users: mustMap(t, map[string]int64{})},
		},
		{name: "nil plan", caller: caller, plan: nil, want: 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := powerLevelsSelfLockoutWarnings(c.caller, c.plan, c.prior)
			if len(got) != c.want {
				t.Fatalf("got %d warnings, want %d: %v", len(got), c.want, got)
			}
			if c.want == 0 {
				return
			}
			if !strings.Contains(got[0], "lock you out") {
				t.Errorf("warning should mention lockout; got %q", got[0])
			}
			if !strings.Contains(got[0], c.wantSub) {
				t.Errorf("warning should contain %q; got %q", c.wantSub, got[0])
			}
			if !strings.Contains(got[0], "room version 12") {
				t.Errorf("warning should keep the room version 12 note; got %q", got[0])
			}
		})
	}
}

// TestCallerLevelIn checks the level lookup against the Matrix auth rules:
// users[sender], then users_default, then 0.
func TestCallerLevelIn(t *testing.T) {
	const caller = "@bot:example.com"
	cases := []struct {
		name       string
		model      *powerLevelsModel
		want       int64
		wantListed bool
	}{
		{
			name:  "listed wins over users_default",
			model: &powerLevelsModel{Users: mustMap(t, map[string]int64{caller: 25}), UsersDefault: types.Int64Value(50)},
			want:  25, wantListed: true,
		},
		{
			name:  "falls back to users_default",
			model: &powerLevelsModel{Users: mustMap(t, map[string]int64{"@alice:example.com": 100}), UsersDefault: types.Int64Value(50)},
			want:  50, wantListed: false,
		},
		{
			name:  "falls back to the spec default of 0",
			model: &powerLevelsModel{Users: types.MapNull(types.Int64Type), UsersDefault: types.Int64Null()},
			want:  0, wantListed: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, listed := callerLevelIn(c.model, caller)
			if got != c.want || listed != c.wantListed {
				t.Errorf("got (%d, %v), want (%d, %v)", got, listed, c.want, c.wantListed)
			}
		})
	}
}

// TestPowerLevelsSchemaKnobsAreOptionalComputed guards the shape the whole
// resource depends on. If a knob loses Computed, plans show perpetual drift
// again (the bug fixed in v0.3.1); if it loses UseStateForUnknown, every plan
// shows it as "known after apply".
func TestPowerLevelsSchemaKnobsAreOptionalComputed(t *testing.T) {
	r := &powerLevelsResource{}
	resp := &fwresource.SchemaResponse{}
	r.Schema(context.Background(), fwresource.SchemaRequest{}, resp)

	for _, name := range powerLevelsKnobs {
		attr, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Errorf("schema is missing %q", name)
			continue
		}
		if !attr.IsOptional() || !attr.IsComputed() {
			t.Errorf("%q: optional=%v computed=%v, want both true", name, attr.IsOptional(), attr.IsComputed())
		}
		switch a := attr.(type) {
		case schema.Int64Attribute:
			if len(a.PlanModifiers) != 1 {
				t.Errorf("%q: got %d plan modifiers, want 1 (UseStateForUnknown)", name, len(a.PlanModifiers))
			}
		case schema.MapAttribute:
			if len(a.PlanModifiers) != 1 {
				t.Errorf("%q: got %d plan modifiers, want 1 (UseStateForUnknown)", name, len(a.PlanModifiers))
			}
		default:
			t.Errorf("%q: unexpected attribute type %T", name, attr)
		}
	}
}

// ---------------------------------------------------------------------------
// Acceptance tests. These need TF_ACC and a live homeserver; resource.Test
// skips itself otherwise, so `go test ./...` on a PR is unaffected.
// ---------------------------------------------------------------------------

func testAccPowerLevelsConfig(kind, eventsDefault string) string {
	// users is deliberately NOT declared here. A declared users map replaces the
	// map wholesale even after the fix, so it would still demote the caller —
	// that case is covered by TestPowerLevelsSelfLockoutWarnings, not here.
	return fmt.Sprintf(`
terraform {
  required_providers {
    matrix = { source = "raspbeguy/matrix" }
  }
}

provider "matrix" {}

resource "matrix_%[1]s" "test" {
  name   = "tf-acc-power-levels"
  topic  = "Managed by the acceptance suite"
  preset = "private_chat"

  # Pinned on purpose. From room version 12 the creator's power comes from
  # m.room.create and the creator is absent from the users map, so the users
  # assertions below would not hold. Issue #29 is about rooms whose creator
  # sits in that map, which is version 11 and earlier.
  room_version = "11"
}

resource "matrix_room_power_levels" "test" {
  room_id        = matrix_%[1]s.test.id
  events_default = %[2]s
}
`, kind, eventsDefault)
}

// testAccCheckServerUserLevel asserts on the homeserver, not on Terraform
// state. State can be right while the event on the server is wrong.
//
// It reads pl.Users directly rather than calling GetUserLevel. A content
// fetched through getState carries no CreateEvent, so mautrix cannot detect
// creator power and GetUserLevel would report users_default for an account
// that actually holds infinite power. The configs pin room version 11, where
// the creator is an ordinary entry in the map.
func testAccCheckServerUserLevel(t *testing.T, roomResourceName, userID string, want int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[roomResourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", roomResourceName)
		}
		pl, found, err := fetchPowerLevels(context.Background(), testAccClient(t), id.RoomID(rs.Primary.ID))
		if err != nil {
			return fmt.Errorf("read power levels: %w", err)
		}
		if !found {
			return fmt.Errorf("room %s has no m.room.power_levels", rs.Primary.ID)
		}
		got, listed := pl.Users[id.UserID(userID)]
		if !listed {
			return fmt.Errorf("server-side users map has no entry for %s, it holds %v (issue #29)", userID, pl.Users)
		}
		if got != want {
			return fmt.Errorf("server-side level for %s: got %d, want %d (issue #29)", userID, got, want)
		}
		return nil
	}
}

// TestAccRoomPowerLevels_PartialConfig reproduces both defects of issue #29.
//
// Defect 1: a create whose config omits Optional+Computed attributes wrote
// unknown values to state and failed with "Provider returned invalid result
// object after apply". Step 1 completing at all is the assertion.
//
// Defect 2: that same create replaced the whole m.room.power_levels event,
// dropping the creating account from its users entry of 100 to users_default 0.
// That is below state_default 50, so the room became unmanageable for good.
func TestAccRoomPowerLevels_PartialConfig(t *testing.T) {
	testAccSkipUnlessAcc(t)
	caller := testAccUserID(t)
	const name = "matrix_room_power_levels.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testAccPowerLevelsConfig("room", "25"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(name, tfjsonpath.New("events_default"), knownvalue.Int64Exact(25)),
					// Defect 1: these were unknown after apply.
					statecheck.ExpectKnownValue(name, tfjsonpath.New("state_default"), knownvalue.Int64Exact(50)),
					statecheck.ExpectKnownValue(name, tfjsonpath.New("users_default"), knownvalue.Int64Exact(0)),
					// Defect 2: the account that created the room keeps level 100.
					statecheck.ExpectKnownValue(name, tfjsonpath.New("users").AtMapKey(caller), knownvalue.Int64Exact(100)),
				},
				Check: testAccCheckServerUserLevel(t, "matrix_room.test", caller, 100),
			},
			// The same config must plan clean after a refresh: no perpetual drift.
			{Config: testAccPowerLevelsConfig("room", "25"), PlanOnly: true},
			// An update must not clobber the untouched fields either. 0 also
			// exercises the omitempty-drops-zero path end to end.
			{
				Config: testAccPowerLevelsConfig("room", "0"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(name, tfjsonpath.New("events_default"), knownvalue.Int64Exact(0)),
					statecheck.ExpectKnownValue(name, tfjsonpath.New("users").AtMapKey(caller), knownvalue.Int64Exact(100)),
				},
				Check: testAccCheckServerUserLevel(t, "matrix_room.test", caller, 100),
			},
			// id equals room_id, so the default import id needs no IdFunc.
			{ResourceName: name, ImportState: true, ImportStateVerify: true},
		},
	})
	// No CheckDestroy: Delete is a deliberate no-op, because power levels
	// cannot be removed from a room.
}

// TestAccRoomPowerLevels_PreservesSpaceDefaults guards the interaction with
// createRoomLike, which creates spaces with events_default 100 and invite 50.
// A power levels resource that declared neither used to reset both to the spec
// defaults, unlocking messages for everyone in the space.
func TestAccRoomPowerLevels_PreservesSpaceDefaults(t *testing.T) {
	testAccSkipUnlessAcc(t)
	const name = "matrix_room_power_levels.test"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Declares events_default only, so invite must survive untouched.
				Config: testAccPowerLevelsConfig("space", "100"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue(name, tfjsonpath.New("events_default"), knownvalue.Int64Exact(100)),
					statecheck.ExpectKnownValue(name, tfjsonpath.New("invite"), knownvalue.Int64Exact(50)),
				},
			},
			{Config: testAccPowerLevelsConfig("space", "100"), PlanOnly: true},
		},
	})
}
