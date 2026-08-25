package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

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
// irreversible half of issue #29: a declared users map replaces the map
// wholesale, so omitting the account the provider runs as demotes it to
// users_default. Below the level needed to send m.room.power_levels the account
// cannot raise itself again, and destroy does not undo it.
func TestPowerLevelsSelfLockoutWarnings(t *testing.T) {
	const caller = "@bot:example.com"
	cases := []struct {
		name   string
		caller string
		model  *powerLevelsModel
		want   int
	}{
		{
			name: "users undeclared is safe", caller: caller, want: 0,
			// The merge in applyPowerLevels keeps the caller's existing entry.
			model: &powerLevelsModel{Users: types.MapNull(types.Int64Type)},
		},
		{
			name: "users unknown is safe", caller: caller, want: 0,
			model: &powerLevelsModel{Users: types.MapUnknown(types.Int64Type)},
		},
		{
			name: "caller listed at 100", caller: caller, want: 0,
			model: &powerLevelsModel{Users: mustMap(t, map[string]int64{caller: 100})},
		},
		{
			name: "others only, falls back to users_default 0", caller: caller, want: 1,
			model: &powerLevelsModel{Users: mustMap(t, map[string]int64{"@alice:example.com": 100})},
		},
		{
			name: "others only, but users_default is high enough", caller: caller, want: 0,
			model: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{"@alice:example.com": 100}),
				UsersDefault: types.Int64Value(100),
			},
		},
		{
			name: "caller listed below state_default", caller: caller, want: 1,
			model: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{caller: 10}),
				StateDefault: types.Int64Value(50),
			},
		},
		{
			name: "others only, but state_default is 0", caller: caller, want: 0,
			model: &powerLevelsModel{
				Users:        mustMap(t, map[string]int64{"@alice:example.com": 100}),
				StateDefault: types.Int64Value(0),
			},
		},
		{
			name: "events override raises the bar", caller: caller, want: 1,
			model: &powerLevelsModel{
				Users:  mustMap(t, map[string]int64{caller: 50}),
				Events: mustMap(t, map[string]int64{"m.room.power_levels": 100}),
			},
		},
		{
			name: "declared empty users map", caller: caller, want: 1,
			model: &powerLevelsModel{Users: mustMap(t, map[string]int64{})},
		},
		{
			name: "no caller known", caller: "", want: 0,
			model: &powerLevelsModel{Users: mustMap(t, map[string]int64{})},
		},
		{name: "nil model", caller: caller, model: nil, want: 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := powerLevelsSelfLockoutWarnings(c.caller, c.model)
			if len(got) != c.want {
				t.Fatalf("got %d warnings, want %d: %v", len(got), c.want, got)
			}
			if c.want > 0 && !strings.Contains(got[0], "lock you out") {
				t.Errorf("warning should mention lockout; got %q", got[0])
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
	resp := &resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, resp)

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
