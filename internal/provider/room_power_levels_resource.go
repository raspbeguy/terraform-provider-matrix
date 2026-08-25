package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var (
	_ resource.Resource                = &powerLevelsResource{}
	_ resource.ResourceWithConfigure   = &powerLevelsResource{}
	_ resource.ResourceWithImportState = &powerLevelsResource{}
	_ resource.ResourceWithModifyPlan  = &powerLevelsResource{}
)

type powerLevelsResource struct{ client *Client }

type powerLevelsModel struct {
	ID            types.String `tfsdk:"id"`
	RoomID        types.String `tfsdk:"room_id"`
	UsersDefault  types.Int64  `tfsdk:"users_default"`
	EventsDefault types.Int64  `tfsdk:"events_default"`
	StateDefault  types.Int64  `tfsdk:"state_default"`
	Ban           types.Int64  `tfsdk:"ban"`
	Kick          types.Int64  `tfsdk:"kick"`
	Invite        types.Int64  `tfsdk:"invite"`
	Redact        types.Int64  `tfsdk:"redact"`
	Users         types.Map    `tfsdk:"users"`
	Events        types.Map    `tfsdk:"events"`
	NotifyRoom    types.Int64  `tfsdk:"notify_room"`
}

func NewRoomPowerLevelsResource() resource.Resource { return &powerLevelsResource{} }

func (r *powerLevelsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_room_power_levels"
}

func (r *powerLevelsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	// All knobs are Optional+Computed: if the user doesn't declare a field, the
	// value the homeserver already has is kept, the next refresh stores it, and
	// UseStateForUnknown keeps subsequent plans from showing perpetual drift
	// (HCL null vs server value). Same pattern as history_visibility.
	intPM := []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}
	mapPM := []planmodifier.Map{mapplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		Description: "Manages the m.room.power_levels state event for a single room. Works on any room-like entity, including spaces — point `room_id` at a matrix_space.id to tune its permissions (e.g. to unlock messages in a space, set `events_default = 0`).\n\n" +
			"Fields you do not declare keep the value the homeserver already has. The provider reads the current event, overlays the fields you declared, and writes the result back. A declared `users` or `events` map is the exception: it replaces that map completely, so that you can remove an entry.\n\n" +
			"**Warning: self-lockout risk.** A `users` map that omits the account the provider runs as drops that account to `users_default`. Below `state_default`, the account can no longer change the room's power levels, and `terraform destroy` does not undo it, because power levels cannot be deleted. Add `(data.matrix_whoami.me.user_id) = 100` to `users`, unless that account created the room in a version 12 room: see below.\n\n" +
			"Dropping `users_default` below `state_default` does the same thing whenever your account has no entry of its own in `users`. Raising `state_default` is not a risk on its own: the homeserver refuses any power value above the sender's own level, so that apply fails instead of locking you out.\n\n" +
			"This applies to accounts whose power comes from `users`. In room version 12 and later, the account that created the room keeps its power whatever `users` says, and must **not** appear in `users` at all: a homeserver rejects any `m.room.power_levels` event that lists a room creator. The provider reports that at plan time.\n\n" +
			"Keys this provider does not model, such as Synapse's `historical`, are preserved. A write merges into the room's current event rather than replacing it.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "Equal to room_id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"room_id": schema.StringAttribute{
				Required: true, Description: "ID of the room or space to manage power levels for.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"users_default":  schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: intPM, Description: "Default level for users not listed in `users`. Matrix default: 0."},
			"events_default": schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: intPM, Description: "Default level to send message events. Matrix default: 0."},
			"state_default":  schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: intPM, Description: "Default level to send state events. Matrix default: 50."},
			"ban":            schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: intPM, Description: "Level required to ban a user. Matrix default: 50."},
			"kick":           schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: intPM, Description: "Level required to kick a user. Matrix default: 50."},
			"invite":         schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: intPM, Description: "Level required to invite a user. Matrix default: 0."},
			"redact":         schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: intPM, Description: "Level required to redact another user's event. Matrix default: 50."},
			"users":          schema.MapAttribute{Optional: true, Computed: true, ElementType: types.Int64Type, PlanModifiers: mapPM, Description: "Per-user overrides by mxid. A declared map replaces the whole `users` map on the homeserver, so list the account the provider runs as, or it loses control of the room."},
			"events":         schema.MapAttribute{Optional: true, Computed: true, ElementType: types.Int64Type, PlanModifiers: mapPM, Description: "Per-event-type overrides. A declared map replaces the whole `events` map on the homeserver."},
			"notify_room":    schema.Int64Attribute{Optional: true, Computed: true, PlanModifiers: intPM, Description: "Power level required for @room notifications (notifications.room)."},
		},
	}
}

func (r *powerLevelsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

// applyModelToRawPowerLevels overlays the attributes the plan decided onto raw
// and leaves every other key untouched, including keys mautrix does not model.
//
// m.room.power_levels has no partial-update semantics: sendState replaces the
// whole event. Any key this function does not write must already be in raw, or
// the homeserver loses it. See issues #29 and #37.
//
// A declared users/events map replaces the whole map, so that entries can be
// removed. That is also why a users map that omits the caller locks it out —
// see powerLevelsSelfLockoutWarnings.
func applyModelToRawPowerLevels(ctx context.Context, raw map[string]json.RawMessage, m *powerLevelsModel) error {
	if raw == nil {
		// Writing to a nil map panics, and this cannot allocate one for the
		// caller. fetchPowerLevels guarantees non-nil; refuse rather than take
		// the provider process down if that ever stops being true.
		return errors.New("power levels content is nil")
	}
	// All Int64 fields are Optional+Computed: skip when null (user didn't
	// declare; keep whatever the server has) or unknown (Computed value not yet
	// resolved during Create). Skipping leaves the key alone.
	set := func(key string, field types.Int64) {
		if field.IsNull() || field.IsUnknown() {
			return
		}
		raw[key] = json.RawMessage(strconv.FormatInt(field.ValueInt64(), 10))
	}
	// users_default and events_default are the only keys whose absence and whose
	// zero mean the same thing. Writing an explicit 0 over an absent key would
	// rewrite the power levels of every managed room on the first apply after an
	// upgrade, for no change in meaning, and an explicit value is auth-checked
	// against the sender's own level where an absent one is not. So leave it be.
	setDefault := func(key string, field types.Int64) {
		if field.IsNull() || field.IsUnknown() {
			return
		}
		if _, present := raw[key]; !present && field.ValueInt64() == 0 {
			return
		}
		raw[key] = json.RawMessage(strconv.FormatInt(field.ValueInt64(), 10))
	}
	setMap := func(key string, field types.Map) error {
		if field.IsNull() || field.IsUnknown() {
			return nil
		}
		values := map[string]int64{}
		if diags := field.ElementsAs(ctx, &values, false); diags.HasError() {
			return errorFromDiags(diags)
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			return err
		}
		raw[key] = encoded
		return nil
	}

	setDefault("users_default", m.UsersDefault)
	setDefault("events_default", m.EventsDefault)
	set("state_default", m.StateDefault)
	set("ban", m.Ban)
	set("kick", m.Kick)
	set("invite", m.Invite)
	set("redact", m.Redact)
	if err := setMap("users", m.Users); err != nil {
		return err
	}
	if err := setMap("events", m.Events); err != nil {
		return err
	}
	if !m.NotifyRoom.IsNull() && !m.NotifyRoom.IsUnknown() {
		setNotificationRoom(raw, m.NotifyRoom.ValueInt64())
	}
	return nil
}

// setNotificationRoom sets notifications.room and keeps every sibling key.
//
// Never fails. Room versions before 10 allow notifications to hold something
// that is not an object, null included, so anything that does not decode as one
// is replaced outright, which is what the typed path always did.
func setNotificationRoom(raw map[string]json.RawMessage, level int64) {
	encoded := json.RawMessage(strconv.FormatInt(level, 10))
	notifications := map[string]json.RawMessage{}
	if existing, ok := raw["notifications"]; ok {
		if err := json.Unmarshal(existing, &notifications); err != nil || notifications == nil {
			notifications = map[string]json.RawMessage{}
		}
	}
	notifications["room"] = encoded
	if merged, err := json.Marshal(notifications); err == nil {
		raw["notifications"] = merged
	} else {
		raw["notifications"] = json.RawMessage(`{"room":` + string(encoded) + `}`)
	}
}

func modelFromPowerLevels(ctx context.Context, pl *event.PowerLevelsEventContent, m *powerLevelsModel) error {
	m.UsersDefault = types.Int64Value(int64(pl.UsersDefault))
	m.EventsDefault = types.Int64Value(int64(pl.EventsDefault))
	if pl.StateDefaultPtr != nil {
		m.StateDefault = types.Int64Value(int64(*pl.StateDefaultPtr))
	} else {
		m.StateDefault = types.Int64Null()
	}
	if pl.BanPtr != nil {
		m.Ban = types.Int64Value(int64(*pl.BanPtr))
	} else {
		m.Ban = types.Int64Null()
	}
	if pl.KickPtr != nil {
		m.Kick = types.Int64Value(int64(*pl.KickPtr))
	} else {
		m.Kick = types.Int64Null()
	}
	if pl.InvitePtr != nil {
		m.Invite = types.Int64Value(int64(*pl.InvitePtr))
	} else {
		m.Invite = types.Int64Null()
	}
	if pl.RedactPtr != nil {
		m.Redact = types.Int64Value(int64(*pl.RedactPtr))
	} else {
		m.Redact = types.Int64Null()
	}
	// `users = {}` and an absent users key are the same thing on the wire: the
	// field is omitempty, so an empty map is never written. Keep a declared
	// empty map rather than flipping it to null, because the flip would show a
	// diff on every plan, forever.
	if len(pl.Users) == 0 {
		if !isKnownEmptyMap(m.Users) {
			m.Users = types.MapNull(types.Int64Type)
		}
	} else {
		raw := map[string]int64{}
		for k, v := range pl.Users {
			raw[string(k)] = int64(v)
		}
		val, d := types.MapValueFrom(ctx, types.Int64Type, raw)
		if d.HasError() {
			return errorFromDiags(d)
		}
		m.Users = val
	}
	if len(pl.Events) == 0 {
		if !isKnownEmptyMap(m.Events) {
			m.Events = types.MapNull(types.Int64Type)
		}
	} else {
		raw := map[string]int64{}
		for k, v := range pl.Events {
			raw[k] = int64(v)
		}
		val, d := types.MapValueFrom(ctx, types.Int64Type, raw)
		if d.HasError() {
			return errorFromDiags(d)
		}
		m.Events = val
	}
	if pl.Notifications != nil && pl.Notifications.RoomPtr != nil {
		m.NotifyRoom = types.Int64Value(int64(*pl.Notifications.RoomPtr))
	} else {
		m.NotifyRoom = types.Int64Null()
	}
	return nil
}

// isKnownEmptyMap reports whether v is a known map with no elements, that is,
// the practitioner wrote `x = {}` instead of leaving the attribute out.
func isKnownEmptyMap(v types.Map) bool {
	return !v.IsNull() && !v.IsUnknown() && len(v.Elements()) == 0
}

// fetchPowerLevels reads the room's current m.room.power_levels as the raw JSON
// object. An absent event gives an empty map and found=false, not an error.
//
// Raw rather than typed. event.PowerLevelsEventContent models ten keys, and
// sendState replaces the whole event, so a typed round trip silently drops
// everything else. Rooms really do carry more: Synapse writes "historical", and
// NotificationPowerLevels models only "room". See issue #37.
//
// The map is never nil. getState leaves out untouched on a 404, and a JSON null
// body makes encoding/json zero the destination. A nil map reads fine and panics
// on write, which would take the whole provider process down.
func fetchPowerLevels(ctx context.Context, c *Client, roomID id.RoomID) (map[string]json.RawMessage, bool, error) {
	raw := map[string]json.RawMessage{}
	found, err := getState(ctx, c, roomID, event.StatePowerLevels, "", &raw)
	if err != nil {
		return map[string]json.RawMessage{}, false, err
	}
	if raw == nil {
		raw = map[string]json.RawMessage{}
	}
	return raw, found, nil
}

// typedPowerLevels decodes a raw power levels object into the struct the model
// mapping needs. Never returns a nil struct without an error.
//
// This fails on exactly the contents the old typed read already failed on: room
// versions before 10 allow a power level to be a JSON string, and versions
// before 6 allow a float.
func typedPowerLevels(raw map[string]json.RawMessage) (*event.PowerLevelsEventContent, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	pl := &event.PowerLevelsEventContent{}
	if err := json.Unmarshal(encoded, pl); err != nil {
		return nil, fmt.Errorf("power levels content is not integer-only, which room versions before 10 allow: %w", err)
	}
	return pl, nil
}

// resolveUnknownPowerLevels fills in only the attributes the plan left unknown,
// taking them from the content the room now holds.
//
// Attributes the plan already decided are kept exactly as they are: Terraform
// rejects an applied value that differs from a known planned value, and a
// planned null is a known value. Without this, Create writes unknowns to state
// and every apply fails. See issue #29.
func resolveUnknownPowerLevels(ctx context.Context, pl *event.PowerLevelsEventContent, m *powerLevelsModel) error {
	var server powerLevelsModel
	if err := modelFromPowerLevels(ctx, pl, &server); err != nil {
		return err
	}
	if m.UsersDefault.IsUnknown() {
		m.UsersDefault = server.UsersDefault
	}
	if m.EventsDefault.IsUnknown() {
		m.EventsDefault = server.EventsDefault
	}
	if m.StateDefault.IsUnknown() {
		m.StateDefault = server.StateDefault
	}
	if m.Ban.IsUnknown() {
		m.Ban = server.Ban
	}
	if m.Kick.IsUnknown() {
		m.Kick = server.Kick
	}
	if m.Invite.IsUnknown() {
		m.Invite = server.Invite
	}
	if m.Redact.IsUnknown() {
		m.Redact = server.Redact
	}
	if m.Users.IsUnknown() {
		m.Users = server.Users
	}
	if m.Events.IsUnknown() {
		m.Events = server.Events
	}
	if m.NotifyRoom.IsUnknown() {
		m.NotifyRoom = server.NotifyRoom
	}
	return nil
}

// applyPowerLevels writes the plan to the room: read the current content,
// overlay what the plan decided, send the merged event, then resolve the
// attributes the plan left unknown from that merged content.
//
// The read is not optional. sendState replaces m.room.power_levels wholesale,
// so a content built from a partial config alone drops every value the config
// does not mention, including the caller's own users entry and the space
// defaults from createRoomLike. See issue #29.
//
// Resolving from the merged content instead of from a second read is
// deliberate: Matrix stores state event content verbatim, so a read-back
// returns what was just sent, one round trip later and open to a
// read-your-writes race on a homeserver with separate read workers.
//
// The merge is over the raw JSON object, so keys mautrix does not model survive
// the write. See issue #37.
func (r *powerLevelsResource) applyPowerLevels(ctx context.Context, m *powerLevelsModel, diags *diag.Diagnostics) {
	roomID := id.RoomID(m.RoomID.ValueString())
	merged, _, err := fetchPowerLevels(ctx, r.client, roomID)
	if err != nil {
		diags.AddError("Failed to read m.room.power_levels", err.Error())
		return
	}
	if err := applyModelToRawPowerLevels(ctx, merged, m); err != nil {
		diags.AddError("Invalid power_levels attributes", err.Error())
		return
	}
	// Decode before sending, not after. A decode failure after a successful send
	// leaves the event in the room and no state in Terraform, which orphans the
	// resource; failing here costs nothing.
	typed, err := typedPowerLevels(merged)
	if err != nil {
		// A distinct summary on purpose: nothing has been sent yet, so the room
		// is untouched, and "failed to map into state" would suggest otherwise.
		diags.AddError("Unsupported power levels content", err.Error())
		return
	}
	if err := sendState(ctx, r.client, roomID, event.StatePowerLevels, "", merged); err != nil {
		diags.AddError("Failed to set m.room.power_levels", err.Error())
		return
	}
	if err := resolveUnknownPowerLevels(ctx, typed, m); err != nil {
		diags.AddError("Failed to map power_levels into state", err.Error())
	}
}

// callerUserID returns the mxid the provider authenticates as, or "" when the
// client isn't configured yet (the framework's early-plan pass).
func callerUserID(c *Client) string {
	if c == nil || c.MX == nil {
		return ""
	}
	return string(c.MX.UserID)
}

// firstKnownInt64 returns the first value that is neither null nor unknown, or
// fallback when there is none. The order expresses the merge in
// applyPowerLevels: what the plan declares wins over what the room holds today,
// and the Matrix spec default is the last resort.
func firstKnownInt64(fallback int64, vals ...types.Int64) int64 {
	for _, v := range vals {
		if !v.IsNull() && !v.IsUnknown() {
			return v.ValueInt64()
		}
	}
	return fallback
}

// mapInt64 looks up a known int64 element of a known map.
func mapInt64(m types.Map, key string) (int64, bool) {
	if m.IsNull() || m.IsUnknown() {
		return 0, false
	}
	raw, ok := m.Elements()[key]
	if !ok {
		return 0, false
	}
	v, ok := raw.(types.Int64)
	if !ok || v.IsNull() || v.IsUnknown() {
		return 0, false
	}
	return v.ValueInt64(), true
}

// callerLevelIn returns the level caller holds in m, and whether the users map
// gives it an entry of its own. Matches the Matrix auth rules: users[sender],
// then users_default, then 0.
func callerLevelIn(m *powerLevelsModel, caller string) (int64, bool) {
	if level, listed := mapInt64(m.Users, caller); listed {
		return level, true
	}
	return firstKnownInt64(0, m.UsersDefault), false
}

// powerLevelsSendBar returns the level needed to send m.room.power_levels once
// the plan is applied. A declared events entry wins over state_default, and what
// the plan declares wins over what the room holds today.
func powerLevelsSendBar(plan, prior *powerLevelsModel) int64 {
	if v, ok := mapInt64(plan.Events, event.StatePowerLevels.Type); ok {
		return v
	}
	// A declared events map replaces the map wholesale, so the room's own entry
	// only survives when the plan leaves events undeclared.
	if prior != nil && (plan.Events.IsNull() || plan.Events.IsUnknown()) {
		if v, ok := mapInt64(prior.Events, event.StatePowerLevels.Type); ok {
			return v
		}
	}
	if prior != nil {
		return firstKnownInt64(50, plan.StateDefault, prior.StateDefault)
	}
	return firstKnownInt64(50, plan.StateDefault)
}

// powerLevelsSelfLockoutWarnings returns human-readable warnings describing how
// the plan would leave the provider's own account unable to manage the room, so
// callers can surface them as plan-time diagnostics. Pure function — no
// client/network access.
//
// Two shapes lock the caller out:
//
//   - A declared users map replaces the map wholesale, so a caller the map omits
//     falls back to users_default.
//   - A raised state_default, or a raised events["m.room.power_levels"], moves
//     the bar above the level the caller already holds. The send still passes,
//     because the homeserver checks it against the old bar. See issue #33.
//
// prior is what the room holds today: the prior state on an update, or a read
// from the homeserver on a create. It is nil when neither is available, and the
// second shape then cannot be detected.
//
// Below the level needed to send m.room.power_levels the result is irreversible.
// The account can't raise itself again, and destroy doesn't undo it, because
// power levels can't be deleted.
//
// Unknown plan values fall back to prior, then to the Matrix spec defaults,
// which is the conservative reading.
//
// Scope: this is about accounts whose power comes from the users map. From room
// version 12 a room's creator holds power through m.room.create instead and is
// absent from the map, so it can't be demoted this way. The room version isn't
// knowable at plan time, so the warning names that case rather than guessing.
func powerLevelsSelfLockoutWarnings(caller string, plan, prior *powerLevelsModel) []string {
	if caller == "" || plan == nil {
		return nil
	}

	// The level the caller holds once the merge in applyPowerLevels has run.
	var level int64
	var listed, declared bool
	switch {
	case !plan.Users.IsNull() && !plan.Users.IsUnknown():
		declared = true
		level, listed = callerLevelIn(plan, caller)
	case prior != nil:
		// users undeclared: the merge keeps the map the room already has, but a
		// declared users_default still replaces the fallback.
		level, listed = mapInt64(prior.Users, caller)
		if !listed {
			level = firstKnownInt64(0, plan.UsersDefault, prior.UsersDefault)
		}
	default:
		return nil // nothing to compare the bar against
	}

	required := powerLevelsSendBar(plan, prior)
	if level >= required {
		return nil
	}

	if prior != nil {
		priorLevel, _ := callerLevelIn(prior, caller)
		if priorLevel < powerLevelsSendBar(prior, nil) {
			// The caller already cannot send m.room.power_levels. This plan is
			// not what locked it out, and the homeserver rejects the send with
			// M_FORBIDDEN, which says so plainly enough.
			return nil
		}
		if level >= priorLevel {
			// The caller's level did not drop, so the only way to reach here is
			// a raised bar. The auth rules refuse any new power value above the
			// sender's own level, so that apply is rejected rather than applied.
			// A raise alone cannot lock anybody out.
			return nil
		}
	}

	// From room version 12 the creator's power comes from m.room.create, not
	// from this map, so the creator cannot be demoted by it. ModifyPlan checks
	// that before it surfaces a warning, but the check needs a readable room, so
	// keep saying it here for the cases where the read fails.
	const creatorNote = " In room version 12 and later the account that created the room keeps its power whatever `users` says, so this does not apply to a room's own creator."

	var reason string
	switch {
	case declared && listed:
		reason = fmt.Sprintf("`users` puts your own account %q at level %d", caller, level)
	case declared:
		reason = fmt.Sprintf("`users` does not list your own account %q, so it falls back to users_default %d", caller, level)
	case !listed:
		reason = fmt.Sprintf("`users_default` leaves your own account %q at level %d, and `users` gives it no entry of its own", caller, level)
	default:
		reason = fmt.Sprintf("your own account %q ends up at level %d", caller, level)
	}
	msg := fmt.Sprintf(
		"%s. That is below the level %d needed to send m.room.power_levels. Applying this will lock you out of the room's power levels, and only a user at a higher level can undo it.",
		reason, required)
	if !listed {
		msg += fmt.Sprintf(" Add %q to `users` (data.matrix_whoami.me.user_id).", caller)
	}
	return []string{msg + creatorNote}
}

// ModifyPlan surfaces self-lockout warnings at plan time, so practitioners see
// them before they run apply. Runs on both create and update plans; skipped on
// destroy.
func (r *powerLevelsResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy plan — nothing to warn about
	}
	var plan powerLevelsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	caller := callerUserID(r.client)
	declared := !plan.Users.IsNull() && !plan.Users.IsUnknown()
	warnings := powerLevelsSelfLockoutWarnings(caller, &plan, r.priorPowerLevels(ctx, req, &plan))
	if !declared && len(warnings) == 0 {
		// Neither check below can fire, so do not pay for the create event.
		return
	}
	creators, privileged := r.roomCreators(ctx, &plan)
	if privileged && declared {
		for _, creator := range creatorsInUsers(plan.Users, creators) {
			resp.Diagnostics.AddAttributeError(path.Root("users"), "Room creator listed in users",
				fmt.Sprintf("`users` lists %q, which created this room. From room version 12 a homeserver "+
					"rejects any m.room.power_levels event that lists a room creator, so this apply cannot "+
					"succeed. A creator keeps its power without an entry: remove %q from `users`.",
					creator, creator))
		}
	}
	if privileged && slices.Contains(creators, id.UserID(caller)) {
		// A creator's power comes from m.room.create, so no users map can take
		// it away. Nothing to warn about.
		return
	}
	for _, w := range warnings {
		resp.Diagnostics.AddWarning("Potential power levels self-lockout", w)
	}
}

// creatorsInUsers returns the creators that a declared users map lists. Pure
// function — no client/network access.
func creatorsInUsers(users types.Map, creators []id.UserID) []id.UserID {
	if users.IsNull() || users.IsUnknown() {
		return nil
	}
	var listed []id.UserID
	for _, creator := range creators {
		if _, ok := users.Elements()[string(creator)]; ok {
			listed = append(listed, creator)
		}
	}
	return listed
}

// roomCreators returns the accounts whose power comes from m.room.create rather
// than from the power levels event, and whether the room version grants them
// that. From room version 12 those accounts hold power no users map can take
// away, and the auth rules skip every power level check for them. They must also
// not appear in users at all: the homeserver rejects an event that lists one.
//
// Best effort: an unreadable room returns nothing, which costs a warning that
// may not apply and skips the creator error. The warning text names that case.
func (r *powerLevelsResource) roomCreators(ctx context.Context, plan *powerLevelsModel) ([]id.UserID, bool) {
	if r.client == nil || plan.RoomID.IsNull() || plan.RoomID.IsUnknown() {
		return nil, false
	}
	// FullStateEvent rather than getCreateContent: this needs the create event's
	// sender, and only the whole event carries it. That endpoint depends on a
	// ?format=event query parameter which is not in the spec, so a homeserver
	// without it just means no suppression, and the warning text says so.
	evt, err := r.client.MX.FullStateEvent(ctx, id.RoomID(plan.RoomID.ValueString()), event.StateCreate, "")
	if err != nil || evt == nil {
		return nil, false
	}
	create := evt.Content.AsCreate()
	if create == nil || !create.RoomVersion.PrivilegedRoomCreators() {
		return nil, false
	}
	return append([]id.UserID{evt.Sender}, create.AdditionalCreators...), true
}

// priorPowerLevels returns what the room holds today: the prior state on an
// update, or a read from the homeserver on a create. Returns nil when neither is
// available.
//
// The create-time read is what lets the check see a resource newly pointed at a
// room that already exists, which is the reported case in issue #33. It is
// skipped while the room itself is still being created: room_id is unknown then,
// and the caller is the room's own creator.
//
// Best effort throughout. A failure here costs a warning, and Create surfaces
// any real error. Nothing here ever touches a planned value, so a homeserver
// that changes between plan and apply cannot produce an inconsistent plan.
func (r *powerLevelsResource) priorPowerLevels(ctx context.Context, req resource.ModifyPlanRequest, plan *powerLevelsModel) *powerLevelsModel {
	var prior powerLevelsModel
	if !req.State.Raw.IsNull() {
		if diags := req.State.Get(ctx, &prior); diags.HasError() {
			return nil
		}
		// room_id forces replacement, so on a replace the prior state describes
		// a different room. Read the planned room instead.
		if prior.RoomID.Equal(plan.RoomID) {
			return &prior
		}
		prior = powerLevelsModel{}
	}
	if r.client == nil || plan.RoomID.IsNull() || plan.RoomID.IsUnknown() {
		return nil
	}
	raw, found, err := fetchPowerLevels(ctx, r.client, id.RoomID(plan.RoomID.ValueString()))
	if err != nil || !found {
		return nil
	}
	pl, err := typedPowerLevels(raw)
	if err != nil {
		return nil
	}
	if err := modelFromPowerLevels(ctx, pl, &prior); err != nil {
		return nil
	}
	return &prior
}

func (r *powerLevelsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan powerLevelsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.applyPowerLevels(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = plan.RoomID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *powerLevelsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state powerLevelsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	raw, found, err := fetchPowerLevels(ctx, r.client, id.RoomID(state.RoomID.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Failed to read m.room.power_levels", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	pl, err := typedPowerLevels(raw)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read m.room.power_levels", err.Error())
		return
	}
	if err := modelFromPowerLevels(ctx, pl, &state); err != nil {
		resp.Diagnostics.AddError("Failed to map power_levels into state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *powerLevelsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior powerLevelsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	r.applyPowerLevels(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *powerLevelsResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Power levels can't be deleted from a room; destroy drops only the state tracking.
}

func (r *powerLevelsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("room_id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
