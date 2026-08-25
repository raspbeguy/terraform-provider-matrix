package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
// the way a real room or a space created by createRoomLike looks. It carries two
// keys mautrix does not model, which is what issue #37 is about: Synapse writes
// historical, and real rooms carry m.call.invite.
func populatedRaw(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"users": {"@bot:example.com": 100},
		"users_default": 5,
		"events": {"m.room.name": 50},
		"events_default": 100,
		"invite": 50, "kick": 60, "ban": 70, "redact": 80, "state_default": 90,
		"notifications": {"room": 95, "org.example.custom": 42},
		"historical": 100,
		"m.call.invite": 50
	}`), &raw); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return raw
}

func populatedContent(t *testing.T) *event.PowerLevelsEventContent {
	t.Helper()
	pl, err := typedPowerLevels(populatedRaw(t))
	if err != nil {
		t.Fatalf("typedPowerLevels: %v", err)
	}
	return pl
}

func rawKey(t *testing.T, raw map[string]json.RawMessage, key string) string {
	t.Helper()
	v, ok := raw[key]
	if !ok {
		return ""
	}
	return string(v)
}

// sameJSON compares two JSON documents by value. Untouched keys keep whatever
// bytes the homeserver sent, whitespace included, which is the point of using
// json.RawMessage; the tests that care about a value must not depend on it.
func sameJSON(t *testing.T, got, want string) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		t.Fatalf("want is not valid JSON: %s", want)
	}
	return reflect.DeepEqual(a, b)
}

// TestApplyModelToRawPowerLevels_PreservesUnmodelledKeys is the regression test
// for issue #37. event.PowerLevelsEventContent models ten keys, and sendState
// replaces the whole event, so a typed round trip dropped everything else.
// Rooms this provider creates carry historical and m.call.invite from the start.
func TestApplyModelToRawPowerLevels_PreservesUnmodelledKeys(t *testing.T) {
	raw := populatedRaw(t)
	m := nothingDeclared()
	m.EventsDefault = types.Int64Value(25)

	if err := applyModelToRawPowerLevels(context.Background(), raw, m); err != nil {
		t.Fatalf("applyModelToRawPowerLevels: %v", err)
	}
	if got := rawKey(t, raw, "historical"); got != "100" {
		t.Errorf("historical: got %q, want 100 (issue #37)", got)
	}
	if got := rawKey(t, raw, "m.call.invite"); got != "50" {
		t.Errorf("m.call.invite: got %q, want 50 (issue #37)", got)
	}
	if got := rawKey(t, raw, "events_default"); got != "25" {
		t.Errorf("events_default: got %q, want the declared 25", got)
	}
}

// TestApplyModelToRawPowerLevels_PreservesUndeclaredFields is the regression
// test for the second defect in issue #29: a partial config used to wipe every
// value it did not mention, including the creating account's users entry and the
// space defaults from createRoomLike.
func TestApplyModelToRawPowerLevels_PreservesUndeclaredFields(t *testing.T) {
	raw := populatedRaw(t)
	m := nothingDeclared()
	m.UsersDefault = types.Int64Value(20)

	if err := applyModelToRawPowerLevels(context.Background(), raw, m); err != nil {
		t.Fatalf("applyModelToRawPowerLevels: %v", err)
	}
	for key, want := range map[string]string{
		"users":          `{"@bot:example.com":100}`,
		"events":         `{"m.room.name":50}`,
		"events_default": "100",
		"invite":         "50",
		"users_default":  "20",
	} {
		if got := rawKey(t, raw, key); !sameJSON(t, got, want) {
			t.Errorf("%s: got %s, want %s", key, got, want)
		}
	}
}

// TestApplyModelToRawPowerLevels_SkipsNullAndUnknown checks the overlay writes
// nothing at all when the practitioner declared nothing.
func TestApplyModelToRawPowerLevels_SkipsNullAndUnknown(t *testing.T) {
	raw := populatedRaw(t)
	before, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := applyModelToRawPowerLevels(context.Background(), raw, nothingDeclared()); err != nil {
		t.Fatalf("applyModelToRawPowerLevels: %v", err)
	}
	after, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("the event changed:\n before %s\n after  %s", before, after)
	}
}

// TestApplyModelToRawPowerLevels_DefaultZeroIsNotWritten guards the one rule
// that keeps this change a no-op migration. An absent users_default and a zero
// mean the same thing, so writing an explicit 0 over an absent key would rewrite
// the power levels of every managed room for no change in meaning. It would also
// be auth-checked against the sender's own level, where an absent key is not.
func TestApplyModelToRawPowerLevels_DefaultZeroIsNotWritten(t *testing.T) {
	raw := map[string]json.RawMessage{"historical": json.RawMessage("100")}
	m := nothingDeclared()
	m.UsersDefault = types.Int64Value(0)
	m.EventsDefault = types.Int64Value(0)

	if err := applyModelToRawPowerLevels(context.Background(), raw, m); err != nil {
		t.Fatalf("applyModelToRawPowerLevels: %v", err)
	}
	if _, present := raw["users_default"]; present {
		t.Error("users_default was written over an absent key, want it left absent")
	}
	if _, present := raw["events_default"]; present {
		t.Error("events_default was written over an absent key, want it left absent")
	}

	// A declared zero over an existing non-zero value must still be written.
	raw = map[string]json.RawMessage{"users_default": json.RawMessage("50")}
	if err := applyModelToRawPowerLevels(context.Background(), raw, m); err != nil {
		t.Fatalf("applyModelToRawPowerLevels: %v", err)
	}
	if got := rawKey(t, raw, "users_default"); got != "0" {
		t.Errorf("users_default: got %q, want the declared 0", got)
	}
}

// TestApplyModelToRawPowerLevels_ZeroIsDeclaredNotSkipped is why the overlay
// reads the types.Int64 rather than a decoded struct: a declared 0 and an
// undeclared field are indistinguishable once mapped through
// event.PowerLevelsEventContent, whose defaults are plain ints.
func TestApplyModelToRawPowerLevels_ZeroIsDeclaredNotSkipped(t *testing.T) {
	raw := populatedRaw(t)
	m := nothingDeclared()
	m.StateDefault = types.Int64Value(0)

	if err := applyModelToRawPowerLevels(context.Background(), raw, m); err != nil {
		t.Fatalf("applyModelToRawPowerLevels: %v", err)
	}
	if got := rawKey(t, raw, "state_default"); got != "0" {
		t.Errorf("state_default: got %q, want the declared 0", got)
	}
}

// TestApplyModelToRawPowerLevels_DeclaredUsersReplacesMap locks in that a
// declared map is authoritative. Merging per key would make it impossible to
// remove a user's level, and it is that wholesale replacement that
// powerLevelsSelfLockoutWarnings warns about.
func TestApplyModelToRawPowerLevels_DeclaredUsersReplacesMap(t *testing.T) {
	raw := populatedRaw(t)
	m := nothingDeclared()
	m.Users = mustMap(t, map[string]int64{"@alice:example.com": 100})

	if err := applyModelToRawPowerLevels(context.Background(), raw, m); err != nil {
		t.Fatalf("applyModelToRawPowerLevels: %v", err)
	}
	if got := rawKey(t, raw, "users"); !sameJSON(t, got, `{"@alice:example.com":100}`) {
		t.Errorf("declared users must replace the map wholesale, got %s", got)
	}
}

// TestApplyModelToRawPowerLevels_DeclaredEmptyMapClears checks that `users = {}`
// clears the map rather than being treated as undeclared.
func TestApplyModelToRawPowerLevels_DeclaredEmptyMapClears(t *testing.T) {
	raw := populatedRaw(t)
	m := nothingDeclared()
	m.Users = mustMap(t, map[string]int64{})

	if err := applyModelToRawPowerLevels(context.Background(), raw, m); err != nil {
		t.Fatalf("applyModelToRawPowerLevels: %v", err)
	}
	if got := rawKey(t, raw, "users"); got != "{}" {
		t.Errorf("users = {} must clear the map, got %s", got)
	}
}

// TestSetNotificationRoom covers the one nested merge. The typed path replaced
// the whole notifications object, losing any sibling key; room versions before
// 10 also allow the value to be something that is not an object at all.
func TestSetNotificationRoom(t *testing.T) {
	cases := []struct {
		name     string
		existing string
		want     string
	}{
		{name: "sibling keys survive", existing: `{"room":50,"org.example.custom":42}`, want: `{"org.example.custom":42,"room":75}`}, //nolint:lll
		{name: "absent object is created", existing: "", want: `{"room":75}`},
		{name: "null is replaced", existing: "null", want: `{"room":75}`},
		{name: "a non-object is replaced", existing: "50", want: `{"room":75}`},
		{name: "an array is replaced", existing: "[1,2]", want: `{"room":75}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := map[string]json.RawMessage{}
			if c.existing != "" {
				raw["notifications"] = json.RawMessage(c.existing)
			}
			setNotificationRoom(raw, 75)
			if got := rawKey(t, raw, "notifications"); !sameJSON(t, got, c.want) {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}

// TestApplyModelToRawPowerLevels_NilMapDoesNotPanic guards the 404 path. getState
// leaves out untouched when the event is absent, and a JSON null body makes
// encoding/json zero the destination. Writing to a nil map panics, which would
// take the whole provider process down, so the overlay refuses one.
//
// fetchPowerLevels is the primary guard and always returns a non-nil map. It
// needs a live client, so this covers the second line of defence.
func TestApplyModelToRawPowerLevels_NilMapDoesNotPanic(t *testing.T) {
	m := nothingDeclared()
	m.StateDefault = types.Int64Value(50)

	var nilMap map[string]json.RawMessage
	if err := applyModelToRawPowerLevels(context.Background(), nilMap, m); err == nil {
		t.Error("want an error for a nil map, not a panic and not a silent no-op")
	}

	// The empty map fetchPowerLevels guarantees works normally.
	raw := map[string]json.RawMessage{}
	if err := applyModelToRawPowerLevels(context.Background(), raw, m); err != nil {
		t.Fatalf("applyModelToRawPowerLevels: %v", err)
	}
	if got := rawKey(t, raw, "state_default"); got != "50" {
		t.Errorf("state_default: got %q, want 50", got)
	}
}

// TestTypedPowerLevels covers the decode that the model mapping needs, including
// the contents room versions before 10 allow and this cannot represent.
func TestTypedPowerLevels(t *testing.T) {
	pl, err := typedPowerLevels(populatedRaw(t))
	if err != nil {
		t.Fatalf("typedPowerLevels: %v", err)
	}
	if pl == nil {
		t.Fatal("typedPowerLevels returned a nil struct with no error")
	}
	if pl.UsersDefault != 5 || pl.Users["@bot:example.com"] != 100 {
		t.Errorf("round trip lost values: %+v", pl)
	}
	if pl.Notifications == nil || pl.Notifications.RoomPtr == nil || *pl.Notifications.RoomPtr != 95 {
		t.Errorf("notifications.room did not round trip: %+v", pl.Notifications)
	}

	// A string power level is legal before room version 10 and cannot decode.
	bad := map[string]json.RawMessage{"users_default": json.RawMessage(`"50"`)}
	if _, err := typedPowerLevels(bad); err == nil {
		t.Error("want an error for a string power level")
	} else if !strings.Contains(err.Error(), "room versions before 10") {
		t.Errorf("the error should name the room version; got %q", err)
	}

	// An empty object is a valid room with everything at its spec default.
	if pl, err := typedPowerLevels(map[string]json.RawMessage{}); err != nil || pl == nil {
		t.Errorf("empty object: pl=%v err=%v", pl, err)
	}
}

// TestCreatorsInUsers guards the room version 12 check. A homeserver rejects any
// m.room.power_levels event that lists a room creator, so a config that does so
// can never apply. The provider used to recommend exactly that.
func TestCreatorsInUsers(t *testing.T) {
	const creator = "@bot:example.com"
	creators := []id.UserID{creator, "@second:example.com"}
	cases := []struct {
		name  string
		users types.Map
		want  int
	}{
		{name: "creator listed", users: mustMap(t, map[string]int64{creator: 100}), want: 1},
		{name: "both creators listed", users: mustMap(t, map[string]int64{creator: 100, "@second:example.com": 100}), want: 2},
		{name: "only others listed", users: mustMap(t, map[string]int64{"@alice:example.com": 100}), want: 0},
		{name: "empty map", users: mustMap(t, map[string]int64{}), want: 0},
		{name: "undeclared", users: types.MapNull(types.Int64Type), want: 0},
		{name: "unknown", users: types.MapUnknown(types.Int64Type), want: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := creatorsInUsers(c.users, creators); len(got) != c.want {
				t.Errorf("got %v, want %d entries", got, c.want)
			}
		})
	}
	if got := creatorsInUsers(mustMap(t, map[string]int64{creator: 100}), nil); len(got) != 0 {
		t.Errorf("no creators means no findings; got %v", got)
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
	if err := resolveUnknownPowerLevels(context.Background(), populatedContent(t), m); err != nil {
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

	if err := resolveUnknownPowerLevels(context.Background(), populatedContent(t), m); err != nil {
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

	if err := resolveUnknownPowerLevels(context.Background(), populatedContent(t), m); err != nil {
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
	if err := modelFromPowerLevels(context.Background(), populatedContent(t), &m); err != nil {
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
		raw, found, err := fetchPowerLevels(context.Background(), testAccClient(t), id.RoomID(rs.Primary.ID))
		if err != nil {
			return fmt.Errorf("read power levels: %w", err)
		}
		if !found {
			return fmt.Errorf("room %s has no m.room.power_levels", rs.Primary.ID)
		}
		pl, err := typedPowerLevels(raw)
		if err != nil {
			return fmt.Errorf("decode power levels: %w", err)
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

// testAccSeedRawPowerLevelsKey adds one key to a room's m.room.power_levels
// through a raw client, so a test does not depend on the homeserver writing it.
// The CI Synapse may not write "historical", and a test that assumed it would be
// silently vacuous.
func testAccSeedRawPowerLevelsKey(t *testing.T, roomResourceName, key, value string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[roomResourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", roomResourceName)
		}
		c := testAccClient(t)
		roomID := id.RoomID(rs.Primary.ID)
		raw, _, err := fetchPowerLevels(context.Background(), c, roomID)
		if err != nil {
			return fmt.Errorf("read power levels: %w", err)
		}
		raw[key] = json.RawMessage(value)
		if err := sendState(context.Background(), c, roomID, event.StatePowerLevels, "", raw); err != nil {
			return fmt.Errorf("seed %s: %w", key, err)
		}
		return nil
	}
}

// testAccCheckServerRawKey asserts on the raw event, not the typed struct that
// caused issue #37, so the assertion cannot share the bug's blind spot.
func testAccCheckServerRawKey(t *testing.T, roomResourceName, key, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[roomResourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", roomResourceName)
		}
		raw, found, err := fetchPowerLevels(context.Background(), testAccClient(t), id.RoomID(rs.Primary.ID))
		if err != nil {
			return fmt.Errorf("read power levels: %w", err)
		}
		if !found {
			return fmt.Errorf("room %s has no m.room.power_levels", rs.Primary.ID)
		}
		got, present := raw[key]
		if !present {
			return fmt.Errorf("a write dropped %q from m.room.power_levels; it holds %v (issue #37)", key, raw)
		}
		if string(got) != want {
			return fmt.Errorf("%s: got %s, want %s", key, got, want)
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
			// Issue #37: seed a key mautrix does not model, so the next apply has
			// something to lose. Seeded rather than trusted, because the CI
			// Synapse may not write "historical" by itself.
			{
				Config: testAccPowerLevelsConfig("room", "25"),
				Check:  testAccSeedRawPowerLevelsKey(t, "matrix_room.test", "historical", "100"),
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
				Check: resource.ComposeAggregateTestCheckFunc(
					testAccCheckServerUserLevel(t, "matrix_room.test", caller, 100),
					// Issue #37: a write used to drop every key mautrix does not
					// model. This apply changed events_default; historical must
					// still be there.
					testAccCheckServerRawKey(t, "matrix_room.test", "historical", "100"),
				),
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
