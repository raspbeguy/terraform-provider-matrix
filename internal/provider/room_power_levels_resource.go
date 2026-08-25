package provider

import (
	"context"
	"fmt"
	"slices"

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
			"**Warning: self-lockout risk.** A `users` map that omits the account the provider runs as drops that account to `users_default`. Below `state_default`, the account can no longer change the room's power levels, and `terraform destroy` does not undo it, because power levels cannot be deleted. Add `(data.matrix_whoami.me.user_id) = 100` to `users`.\n\n" +
			"Dropping `users_default` below `state_default` does the same thing whenever your account has no entry of its own in `users`. Raising `state_default` is not a risk on its own: the homeserver refuses any power value above the sender's own level, so that apply fails instead of locking you out.\n\n" +
			"This applies to accounts whose power comes from `users`. In room version 12 and later, the account that created the room keeps its power whatever `users` says.",
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

// applyModelToPowerLevels overlays the attributes the plan decided onto pl and
// leaves every other field of pl untouched.
//
// m.room.power_levels has no partial-update semantics: sendState replaces the
// whole event. Any field this function does not write must already be present
// in pl, or the homeserver loses it. See issue #29.
//
// A declared users/events map replaces the whole map, so that entries can be
// removed. That is also why a users map that omits the caller locks it out —
// see powerLevelsSelfLockoutWarnings.
func applyModelToPowerLevels(ctx context.Context, pl *event.PowerLevelsEventContent, m *powerLevelsModel) error {
	// All Int64 fields are Optional+Computed: skip when null (user didn't
	// declare; keep whatever the server has) or unknown (Computed value not yet
	// resolved during Create). Skipping leaves the base value in place.
	set := func(field types.Int64) (int, bool) {
		if field.IsNull() || field.IsUnknown() {
			return 0, false
		}
		return int(field.ValueInt64()), true
	}
	if v, ok := set(m.UsersDefault); ok {
		pl.UsersDefault = v
	}
	if v, ok := set(m.EventsDefault); ok {
		pl.EventsDefault = v
	}
	if v, ok := set(m.StateDefault); ok {
		pl.StateDefaultPtr = &v
	}
	if v, ok := set(m.Ban); ok {
		pl.BanPtr = &v
	}
	if v, ok := set(m.Kick); ok {
		pl.KickPtr = &v
	}
	if v, ok := set(m.Invite); ok {
		pl.InvitePtr = &v
	}
	if v, ok := set(m.Redact); ok {
		pl.RedactPtr = &v
	}
	if !m.Users.IsNull() && !m.Users.IsUnknown() {
		raw := map[string]int64{}
		if diags := m.Users.ElementsAs(ctx, &raw, false); diags.HasError() {
			return errorFromDiags(diags)
		}
		pl.Users = make(map[id.UserID]int, len(raw))
		for k, v := range raw {
			pl.Users[id.UserID(k)] = int(v)
		}
	}
	if !m.Events.IsNull() && !m.Events.IsUnknown() {
		raw := map[string]int64{}
		if diags := m.Events.ElementsAs(ctx, &raw, false); diags.HasError() {
			return errorFromDiags(diags)
		}
		pl.Events = make(map[string]int, len(raw))
		for k, v := range raw {
			pl.Events[k] = int(v)
		}
	}
	if v, ok := set(m.NotifyRoom); ok {
		pl.Notifications = &event.NotificationPowerLevels{RoomPtr: &v}
	}
	return nil
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

// fetchPowerLevels reads the room's current m.room.power_levels. An absent
// event gives an empty content and found=false, not an error.
func fetchPowerLevels(ctx context.Context, c *Client, roomID id.RoomID) (*event.PowerLevelsEventContent, bool, error) {
	pl := &event.PowerLevelsEventContent{}
	found, err := getState(ctx, c, roomID, event.StatePowerLevels, "", pl)
	if err != nil {
		return nil, false, err
	}
	return pl, found, nil
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
// Known limit: keys mautrix does not model, such as Synapse's "historical",
// don't survive the typed round trip. That predates this function; a complete
// fix needs a raw JSON merge.
func (r *powerLevelsResource) applyPowerLevels(ctx context.Context, m *powerLevelsModel, diags *diag.Diagnostics) {
	roomID := id.RoomID(m.RoomID.ValueString())
	merged, _, err := fetchPowerLevels(ctx, r.client, roomID)
	if err != nil {
		diags.AddError("Failed to read m.room.power_levels", err.Error())
		return
	}
	if err := applyModelToPowerLevels(ctx, merged, m); err != nil {
		diags.AddError("Invalid power_levels attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, roomID, event.StatePowerLevels, "", merged); err != nil {
		diags.AddError("Failed to set m.room.power_levels", err.Error())
		return
	}
	if err := resolveUnknownPowerLevels(ctx, merged, m); err != nil {
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
	warnings := powerLevelsSelfLockoutWarnings(caller, &plan, r.priorPowerLevels(ctx, req, &plan))
	if len(warnings) == 0 {
		return
	}
	// Only now, because this costs a request: a room creator with privileged
	// creator power cannot be demoted through the users map at all.
	if r.callerHasCreatorPower(ctx, &plan, caller) {
		return
	}
	for _, w := range warnings {
		resp.Diagnostics.AddWarning("Potential power levels self-lockout", w)
	}
}

// callerHasCreatorPower reports whether the caller's power comes from
// m.room.create rather than from the power levels event. From room version 12 a
// room's creators hold power that no users map can take away, and the Matrix
// auth rules skip every power level check for them.
//
// Best effort: an unreadable room returns false, which costs a warning that may
// not apply. The warning text names the same case for that reason.
func (r *powerLevelsResource) callerHasCreatorPower(ctx context.Context, plan *powerLevelsModel, caller string) bool {
	if r.client == nil || caller == "" || plan.RoomID.IsNull() || plan.RoomID.IsUnknown() {
		return false
	}
	// FullStateEvent rather than getCreateContent: this needs the create event's
	// sender, and only the whole event carries it. That endpoint depends on a
	// ?format=event query parameter which is not in the spec, so a homeserver
	// without it just means no suppression, and the warning text says so.
	evt, err := r.client.MX.FullStateEvent(ctx, id.RoomID(plan.RoomID.ValueString()), event.StateCreate, "")
	if err != nil || evt == nil {
		return false
	}
	create := evt.Content.AsCreate()
	if create == nil || !create.RoomVersion.PrivilegedRoomCreators() {
		return false
	}
	return evt.Sender == id.UserID(caller) || slices.Contains(create.AdditionalCreators, id.UserID(caller))
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
	pl, found, err := fetchPowerLevels(ctx, r.client, id.RoomID(plan.RoomID.ValueString()))
	if err != nil || !found {
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
	pl, found, err := fetchPowerLevels(ctx, r.client, id.RoomID(state.RoomID.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Failed to read m.room.power_levels", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
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
