package provider

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var (
	_ resource.Resource                = &roomStateResource{}
	_ resource.ResourceWithConfigure   = &roomStateResource{}
	_ resource.ResourceWithImportState = &roomStateResource{}
	_ resource.ResourceWithModifyPlan  = &roomStateResource{}
)

// stateEventOwners names the resource that owns each state event this provider
// models. matrix_room_state is the escape hatch for everything else, and until
// issue #58 nothing enforced the "everything else".
//
// Two resources cannot own one remote object. Each apply writes one value, each
// refresh reads the other, and the plan never settles. Worse, writing one of
// these here bypasses the guard its own resource carries: the power levels
// self-lockout warning, and the server ACL deny-all refusal.
//
// Keyed by the mautrix constants, so TestStateEventOwnersIsComplete can compare
// this table against what the package actually sends.
var stateEventOwners = map[string]string{
	event.StatePowerLevels.Type:       "matrix_room_power_levels",
	event.StateServerACL.Type:         "matrix_room_server_acl",
	event.StateJoinRules.Type:         "matrix_room_join_rules",
	event.StateMember.Type:            "matrix_room_member or matrix_user_profile_override",
	event.StateSpaceChild.Type:        "matrix_space_child",
	event.StateRoomName.Type:          "matrix_room or matrix_space",
	event.StateTopic.Type:             "matrix_room or matrix_space",
	event.StateRoomAvatar.Type:        "matrix_room or matrix_space",
	event.StateHistoryVisibility.Type: "matrix_room or matrix_space",
	event.StateEncryption.Type:        "matrix_room or matrix_space",
	event.StateCreate.Type:            "matrix_room or matrix_space",
}

// readOnlyStateEvents are events the provider reads but no resource writes, so
// matrix_room_state may manage them. An entry here is a deliberate exemption
// from stateEventOwners and needs a reason.
//
// m.room.canonical_alias: matrix_room exposes it as a computed attribute and
// never writes it, and matrix_room_alias manages the directory mapping rather
// than this event. So nothing else can fight over it, and refusing it would
// leave nobody able to repair a room that advertises a dead alias, which is the
// very thing matrix_room_alias now warns about. See issue #59.
var readOnlyStateEvents = map[string]string{
	event.StateCanonicalAlias.Type: "read by matrix_room, written by no resource",
}

// clearingLocksRoom reports why an event type must never have its content
// cleared, and whether that is so.
//
// Matrix has no way to remove a state event, so Delete publishes empty content.
// That is right for almost everything and catastrophic for two events, because
// the event survives and its absent keys fall back to defaults nobody wants.
func clearingLocksRoom(eventType string) (string, bool) {
	switch eventType {
	case event.StatePowerLevels.Type:
		return "Empty content leaves the event in place, so every level falls back to its " +
			"default: state_default 50 and every user at users_default 0. In a room from " +
			"version 11 or earlier nobody can send a state event again, so nobody can undo it. " +
			"From version 12 the creators keep their power through m.room.create, but this " +
			"provider does not know the room version here and does not gamble on it.", true
	case event.StateServerACL.Type:
		return "Empty content leaves the event in place with no allow list, which denies every " +
			"homeserver. Every remote server rejects this room's events from your server after " +
			"that, and rejects a corrective ACL too, so the room never federates again. See " +
			"issue #57.", true
	}
	return "", false
}

// roomStateOwnershipDiag refuses an event type a typed resource owns.
func roomStateOwnershipDiag(eventType types.String) diag.Diagnostics {
	var diags diag.Diagnostics
	// A type computed from another resource is not known yet, and judging it
	// would refuse a valid configuration. Belt and braces: ValueString returns
	// "" for an unknown or null value and no row is keyed on "", so the lookup
	// below already lets it through. Stated here so the intent survives a change
	// to what an empty event type means.
	if eventType.IsUnknown() || eventType.IsNull() {
		return diags
	}
	name := eventType.ValueString()
	owner, owned := stateEventOwners[name]
	if !owned {
		return diags
	}
	detail := "event_type \"" + name + "\" is managed by " + owner + ". Two resources cannot own " +
		"one event: each apply writes one value, each refresh reads the other, and the plan " +
		"never settles. Managing it here also skips the checks that resource carries. Use " +
		owner + " instead."
	if why, locks := clearingLocksRoom(name); locks {
		detail += "\n\nDestroying this resource would be worse still. " + why
	}
	diags.AddAttributeError(path.Root("event_type"), "This event type belongs to another resource", detail)
	return diags
}

type roomStateResource struct{ client *Client }

type roomStateModel struct {
	ID          types.String `tfsdk:"id"`
	RoomID      types.String `tfsdk:"room_id"`
	EventType   types.String `tfsdk:"event_type"`
	StateKey    types.String `tfsdk:"state_key"`
	ContentJSON types.String `tfsdk:"content_json"`
}

func NewRoomStateResource() resource.Resource { return &roomStateResource{} }

func (r *roomStateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_room_state"
}

func (r *roomStateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "Sends an arbitrary state event. Escape hatch for anything not covered by typed resources.\n\n" +
			"An event type that a typed resource owns is refused. Two resources cannot own one event: each apply writes one value, each refresh reads the other, and the plan never settles. The error names the resource to use.\n\n" +
			"Destroy clears the event content, because Matrix has no way to remove a state event. For `m.room.power_levels` and `m.room.server_acl` that cannot be undone, so this resource refuses to clear them. Use `terraform state rm` on such a resource instead.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "Composite: <room_id>|<event_type>|<state_key>.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"room_id":    schema.StringAttribute{Required: true, PlanModifiers: forceNew},
			"event_type": schema.StringAttribute{Required: true, PlanModifiers: forceNew},
			"state_key":  schema.StringAttribute{Optional: true, Computed: true, PlanModifiers: append(forceNew, stringplanmodifier.UseStateForUnknown()), Description: "Defaults to empty string."},
			"content_json": schema.StringAttribute{
				Required:      true,
				Description:   "JSON-encoded state event content. Compared semantically, so whitespace/key order don't trigger drift.",
				PlanModifiers: []planmodifier.String{jsonSemanticEqualityModifier{}},
			},
		},
	}
}

func (r *roomStateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

// ModifyPlan refuses an event type a typed resource owns.
//
// This lives here rather than in ValidateConfig so that a destroy plan is never
// refused. A resource that predates this check, or one that was imported, has
// to stay destroyable, and its configuration block is still present while a
// practitioner destroys it. Delete carries its own guard for the two events
// whose clearing cannot be undone.
func (r *roomStateResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // a destroy plan writes nothing new
	}
	var plan roomStateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(roomStateOwnershipDiag(plan.EventType)...)
}

func (r *roomStateResource) send(ctx context.Context, m *roomStateModel) error {
	var content any
	if err := json.Unmarshal([]byte(m.ContentJSON.ValueString()), &content); err != nil {
		return err
	}
	return sendState(ctx, r.client, id.RoomID(m.RoomID.ValueString()),
		event.NewEventType(m.EventType.ValueString()), m.StateKey.ValueString(), content)
}

func (r *roomStateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan roomStateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.StateKey.IsNull() || plan.StateKey.IsUnknown() {
		plan.StateKey = types.StringValue("")
	}
	if err := r.send(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to set state event", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.RoomID.ValueString() + "|" + plan.EventType.ValueString() + "|" + plan.StateKey.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomStateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roomStateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var raw map[string]any
	found, err := getState(ctx, r.client, id.RoomID(state.RoomID.ValueString()),
		event.NewEventType(state.EventType.ValueString()), state.StateKey.ValueString(), &raw)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read state event", err.Error())
		return
	}
	if !found || len(raw) == 0 {
		// Empty content means the caller (or another actor) cleared the event; drop it.
		resp.State.RemoveResource(ctx)
		return
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		resp.Diagnostics.AddError("Failed to re-encode state event content", err.Error())
		return
	}
	state.ContentJSON = types.StringValue(string(buf))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *roomStateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior roomStateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	if err := r.send(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to update state event", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomStateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roomStateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// ValidateConfig stops a new resource of this shape. This guards one that is
	// already in state or imported, which is reached by removing the
	// configuration block. See issue #58.
	if why, locks := clearingLocksRoom(state.EventType.ValueString()); locks {
		resp.Diagnostics.AddError("Refusing to clear "+state.EventType.ValueString(),
			why+"\n\nThere is no safe destroy for this event. Use `terraform state rm` to drop "+
				"the resource without touching the homeserver.")
		return
	}
	err := sendState(ctx, r.client, id.RoomID(state.RoomID.ValueString()),
		event.NewEventType(state.EventType.ValueString()), state.StateKey.ValueString(), map[string]any{})
	failedDestroy(&resp.Diagnostics, "Failed to clear state event", err)
}

func (r *roomStateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "|", 3)
	if len(parts) < 2 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <room_id>|<event_type>[|<state_key>]")
		return
	}
	stateKey := ""
	if len(parts) == 3 {
		stateKey = parts[2]
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("room_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("event_type"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("state_key"), stateKey)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
