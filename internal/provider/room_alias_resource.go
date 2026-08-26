package provider

import (
	"context"
	"fmt"
	"slices"

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
	_ resource.Resource                = &roomAliasResource{}
	_ resource.ResourceWithConfigure   = &roomAliasResource{}
	_ resource.ResourceWithImportState = &roomAliasResource{}
	_ resource.ResourceWithModifyPlan  = &roomAliasResource{}
)

type roomAliasResource struct{ client *Client }

type roomAliasModel struct {
	ID     types.String `tfsdk:"id"`
	Alias  types.String `tfsdk:"alias"`
	RoomID types.String `tfsdk:"room_id"`
}

func NewRoomAliasResource() resource.Resource { return &roomAliasResource{} }

func (r *roomAliasResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_room_alias"
}

func (r *roomAliasResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Maps an alias (#name:server) to a room in the homeserver's directory.\n\n" +
			"An alias points at one room, so a change to `room_id` removes the mapping and creates it again. If the second step fails, the provider puts the old mapping back and says so. If that fails too, the alias is gone, and the error says what to do.\n\n" +
			"A refresh that cannot reach the homeserver is an error, not a missing alias. Only a 404 drops the resource from state.\n\n" +
			"Destroying an alias that a room advertises in `m.room.canonical_alias` is usually safe, because a homeserver removes it from that event by itself. It does so only if the account may send the event, and it deletes the alias either way. So the provider warns at plan time when the account lacks the power level, and the room would be left advertising an address that no longer resolves.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "Equal to alias.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"alias": schema.StringAttribute{
				Required: true, Description: "Full alias including #name:server.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"room_id": schema.StringAttribute{Required: true, Description: "Target room ID."},
		},
	}
}

func (r *roomAliasResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

// advertisesAlias reports whether a room's m.room.canonical_alias names the
// alias, either as the canonical one or among the alternates. A homeserver
// treats both the same way when an alias is removed.
func advertisesAlias(alias string, canon *event.CanonicalAliasEventContent) bool {
	if canon == nil {
		return false
	}
	if string(canon.Alias) == alias {
		return true
	}
	return slices.Contains(canon.AltAliases, id.RoomAlias(alias))
}

// canonicalAliasWarning returns what to tell a practitioner who is about to
// destroy an alias the room advertises, or "" when there is nothing to say.
//
// A homeserver removes a deleted alias from m.room.canonical_alias itself:
// Synapse does it in _update_canonical_alias, for both the alias and the
// alt_aliases list. So the usual case needs no warning at all.
//
// It is best effort, though. Synapse catches the AuthError, logs it, and deletes
// the alias anyway. That happens when the caller may delete the alias, because
// it created it, but may not send state events in the room. The room is then
// left advertising an address that no longer resolves. See issue #59.
func canonicalAliasWarning(alias, caller, roomID string, canon *event.CanonicalAliasEventContent, pl *event.PowerLevelsEventContent) string {
	if caller == "" || pl == nil || !advertisesAlias(alias, canon) {
		return ""
	}
	need := pl.GetEventLevel(event.StateCanonicalAlias)
	have := pl.GetUserLevel(id.UserID(caller))
	if have >= need {
		return ""
	}
	return fmt.Sprintf(
		"%s is advertised by %s in m.room.canonical_alias. A homeserver removes a deleted alias "+
			"from that event, but only if the caller may send it. %s has power level %d in this "+
			"room and needs %d, so the homeserver logs the failure and deletes the alias anyway. "+
			"The room then advertises an address that no longer resolves.\n\n"+
			"Raise the level of %s in the room, or correct m.room.canonical_alias afterwards with "+
			"a matrix_room_state resource.",
		alias, roomID, caller, have, need, caller)
}

// ModifyPlan warns before a destroy that would leave the room advertising a
// dead alias.
//
// It runs only on a destroy plan, and stops at the first read for an alias the
// room does not advertise, which is the common case. The reads that follow only
// happen when the warning might fire.
//
// A read that fails produces no warning and no error. An advisory check must
// not fail a plan, and nothing here feeds a value that Terraform will act on.
func (r *roomAliasResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if !req.Plan.Raw.IsNull() || req.State.Raw.IsNull() || r.client == nil {
		return // not a destroy of an existing alias
	}
	var state roomAliasModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	roomID := id.RoomID(state.RoomID.ValueString())
	alias := state.Alias.ValueString()

	var canon event.CanonicalAliasEventContent
	found, err := getState(ctx, r.client, roomID, event.StateCanonicalAlias, "", &canon)
	if err != nil || !found || !advertisesAlias(alias, &canon) {
		return
	}
	caller := callerUserID(r.client)
	if caller == "" {
		return
	}
	if creatorKeepsPower(ctx, r.client, roomID, id.UserID(caller)) {
		// A creator can always send the event, so the homeserver's own cleanup
		// will work. This also covers the case where the answer is unknown,
		// where silence beats a warning that may be wrong.
		return
	}
	var pl event.PowerLevelsEventContent
	if found, err := getState(ctx, r.client, roomID, event.StatePowerLevels, "", &pl); err != nil || !found {
		return
	}
	if msg := canonicalAliasWarning(alias, caller, string(roomID), &canon, &pl); msg != "" {
		resp.Diagnostics.AddWarning("The room will advertise a dead alias", msg)
	}
}

// creatorKeepsPower reports whether the caller's power in this room comes from
// m.room.create rather than from the power levels event, which is true for a
// creator of a room from version 12 onwards.
//
// It answers true when it cannot tell, because every caller uses it to decide
// whether to stay quiet.
func creatorKeepsPower(ctx context.Context, c *Client, roomID id.RoomID, caller id.UserID) bool {
	create, found, err := getCreateContent(ctx, c, roomID)
	if err != nil || !found {
		return true // cannot tell
	}
	if !create.RoomVersion.PrivilegedRoomCreators() {
		return false // creators hold no special power in this room version
	}
	// The creator is the sender of m.room.create, and only the whole event
	// carries a sender. That endpoint needs a query parameter which is not in
	// the specification, so a homeserver without it means we cannot tell.
	evt, err := c.MX.FullStateEvent(ctx, roomID, event.StateCreate, "")
	if err != nil || evt == nil {
		return true
	}
	return evt.Sender == caller || slices.Contains(create.AdditionalCreators, caller)
}

func (r *roomAliasResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan roomAliasModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := r.client.MX.CreateAlias(ctx, id.RoomAlias(plan.Alias.ValueString()), id.RoomID(plan.RoomID.ValueString())); err != nil {
		resp.Diagnostics.AddError("Failed to create alias", err.Error())
		return
	}
	plan.ID = plan.Alias
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomAliasResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roomAliasModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	res, err := r.client.MX.ResolveAlias(ctx, id.RoomAlias(state.Alias.ValueString()))
	if err != nil {
		if notFoundErr(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		// A Read must never drop a resource it could not check. Dropping it on
		// a 502 or a timeout makes the next plan propose a create, which then
		// fails with M_ROOM_IN_USE because the alias was there all along. See
		// issue #59.
		resp.Diagnostics.AddError("Failed to resolve alias", err.Error())
		return
	}
	state.RoomID = types.StringValue(string(res.RoomID))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *roomAliasResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// Only room_id can change (alias is ForceNew). Reassign by delete + create.
	var plan, prior roomAliasModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// An alias that is already gone needs no deleting, which is the rule
	// failedDestroy applies to destroy. It also heals the state a half-finished
	// update leaves behind: the next apply skips the missing delete and creates
	// the alias. See issue #59.
	if _, err := r.client.MX.DeleteAlias(ctx, id.RoomAlias(prior.Alias.ValueString())); err != nil && !notFoundErr(err) {
		resp.Diagnostics.AddError("Failed to update alias (delete step)", err.Error())
		return
	}
	if _, err := r.client.MX.CreateAlias(ctx, id.RoomAlias(plan.Alias.ValueString()), id.RoomID(plan.RoomID.ValueString())); err != nil {
		// The alias is gone at this point, and state still describes the prior
		// mapping. Put it back, so a failed update changes nothing.
		if _, rerr := r.client.MX.CreateAlias(ctx, id.RoomAlias(prior.Alias.ValueString()), id.RoomID(prior.RoomID.ValueString())); rerr != nil {
			resp.Diagnostics.AddError("Failed to update alias, and the old mapping could not be restored",
				"The alias no longer exists on the homeserver, and state still describes the mapping "+
					"it had before. Create it again by hand, or run apply again: the delete step "+
					"tolerates a missing alias, so a later apply can finish the move.\n\n"+
					"Create step: "+err.Error()+"\nRestore step: "+rerr.Error())
			return
		}
		resp.Diagnostics.AddError("Failed to update alias (create step)",
			err.Error()+"\n\nThe previous mapping was restored, so nothing changed.")
		return
	}
	plan.ID = plan.Alias
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomAliasResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roomAliasModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	_, err := r.client.MX.DeleteAlias(ctx, id.RoomAlias(state.Alias.ValueString()))
	failedDestroy(&resp.Diagnostics, "Alias delete failed", err)
}

func (r *roomAliasResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("alias"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
