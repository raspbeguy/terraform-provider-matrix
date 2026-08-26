package provider

import (
	"context"
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
	_ resource.Resource                = &userProfileOverrideResource{}
	_ resource.ResourceWithConfigure   = &userProfileOverrideResource{}
	_ resource.ResourceWithImportState = &userProfileOverrideResource{}
)

type userProfileOverrideResource struct{ client *Client }

type userProfileOverrideModel struct {
	ID          types.String `tfsdk:"id"`
	RoomID      types.String `tfsdk:"room_id"`
	UserID      types.String `tfsdk:"user_id"`
	DisplayName types.String `tfsdk:"display_name"`
	AvatarURL   types.String `tfsdk:"avatar_url"`
}

func NewUserProfileOverrideResource() resource.Resource { return &userProfileOverrideResource{} }

func (r *userProfileOverrideResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_profile_override"
}

func (r *userProfileOverrideResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "Manages a user's per-room profile override (displayname / avatar) by editing the `m.room.member` state event for that user in that room. Common use: a bot that wants different displaynames in different rooms (\"PagerDuty Bot\" in #oncall, \"StatusBot\" in #general). Membership itself must already be set — typically by a `matrix_room_member` resource — this resource only modifies the displayname/avatar fields and preserves whatever membership is currently in place.\n\n" +
			"Permissions: setting your own per-room profile always works. Setting someone else's requires sufficient power level on the m.room.member event.\n\n" +
			"**Ordering with `matrix_user_profile`.** Most homeservers (Synapse included) propagate global profile changes to every `m.room.member` event the user has, which wipes per-room overrides if the global change happens *after* the override. If you manage both, add `depends_on = [matrix_user_profile.<name>]` to this resource so Terraform applies the override last. Without that, you'll see perpetual drift after every apply.\n\n" +
			"**Override persists across leave/rejoin.** Per-room overrides live in the `m.room.member` state event, which sticks around even after the user leaves the room (with `membership = \"leave\"`). If the user later rejoins, the previous displayname/avatar override is still attached. To fully clear an override, destroy this resource before the user leaves.\n\n" +
			"Fields you leave out are not touched. `m.room.member` has no way to say \"inherit the global profile\", so writing an empty value would not restore anything: it removes the field, and clients then show the raw mxid. A homeserver normally copies the global profile into the member event when the user joins, so leaving a field out keeps that value.\n\n" +
			"To remove an override, set the attribute to an empty string and apply. Destroying the resource drops it from state and leaves the `m.room.member` event as it is, for the same reason `matrix_user_profile` refuses to clear a global profile on destroy.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Composite ID: <room_id>|<user_id>.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"room_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew},
			"user_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew},
			"display_name": schema.StringAttribute{
				Optional:    true,
				Description: "Per-room display name. Leave it out and this resource does not touch the field, so whatever the room already shows stays, which is normally the global display name. Set it to an empty string to clear the override.",
			},
			"avatar_url": schema.StringAttribute{
				Optional:    true,
				Description: "Per-room avatar mxc:// URI. Leave it out and this resource does not touch the field, so whatever the room already shows stays, which is normally the global avatar. Set it to an empty string to clear the override.",
			},
		},
	}
}

func (r *userProfileOverrideResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

// overlayProfile applies the declared fields to a member event.
//
// A null attribute is one this resource does not manage, so whatever the event
// holds stays: usually the global value Synapse copied in at join. Writing an
// empty string there, as this used to, does not restore anything. Both fields
// are omitempty, so it removes the key and clients render the raw mxid, which
// stripped the avatar of anyone who declared only a display name. See #39.
//
// An empty string clears the field on purpose. That is the only way to remove an
// override, because destroy leaves the member event alone.
func overlayProfile(m *userProfileOverrideModel, current *event.MemberEventContent) error {
	if !m.DisplayName.IsNull() {
		current.Displayname = m.DisplayName.ValueString()
	}
	if !m.AvatarURL.IsNull() {
		// ParseContentURI accepts an empty string and returns an empty URI, so
		// clearing needs no special case here. The test pins that, because the
		// clear path would break silently if mautrix ever tightened it.
		uri, err := id.ParseContentURI(m.AvatarURL.ValueString())
		if err != nil {
			return err
		}
		current.AvatarURL = uri.CUString()
	}
	return nil
}

// refreshProfileField returns the value to record for a field the configuration
// manages.
//
// A server with no value is ambiguous: it means "cleared" for a configuration
// that asked for that, and "gone" for one that asked for a name. Keep a declared
// empty string rather than flipping it to null, or the plan diffs on every run.
func refreshProfileField(managed types.String, server string) types.String {
	if server != "" {
		return types.StringValue(server)
	}
	if !managed.IsNull() && managed.ValueString() == "" {
		return managed
	}
	return types.StringNull()
}

// applyOverride reads the current m.room.member event, overlays the fields the
// configuration declares, and re-sends the event. Preserves membership and every
// other field the spec defines on m.room.member that the provider doesn't
// manage.
func (r *userProfileOverrideResource) applyOverride(ctx context.Context, m *userProfileOverrideModel, diags *diag.Diagnostics) {
	roomID := id.RoomID(m.RoomID.ValueString())
	userID := m.UserID.ValueString()

	var current event.MemberEventContent
	found, err := getState(ctx, r.client, roomID, event.StateMember, userID, &current)
	if err != nil {
		diags.AddError("Failed to read m.room.member", err.Error())
		return
	}
	if !found {
		diags.AddError("Membership not found",
			"No m.room.member event for "+userID+" in "+m.RoomID.ValueString()+
				" — the user must already be a member (use a matrix_room_member resource first).")
		return
	}

	if err := overlayProfile(m, &current); err != nil {
		diags.AddAttributeError(path.Root("avatar_url"), "Invalid mxc URI", err.Error())
		return
	}

	if err := sendState(ctx, r.client, roomID, event.StateMember, userID, &current); err != nil {
		diags.AddError("Failed to write m.room.member", err.Error())
	}
}

func (r *userProfileOverrideResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userProfileOverrideModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.applyOverride(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(plan.RoomID.ValueString() + "|" + plan.UserID.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userProfileOverrideResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userProfileOverrideModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var member event.MemberEventContent
	found, err := getState(ctx, r.client, id.RoomID(state.RoomID.ValueString()), event.StateMember, state.UserID.ValueString(), &member)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read m.room.member", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	// Only refresh fields the user is actually managing. Synapse auto-fills missing
	// displayname/avatar_url in m.room.member from the user's global profile, so a
	// blind refresh of an unmanaged field would store the global value and produce
	// perpetual drift on every plan.
	if !state.DisplayName.IsNull() {
		state.DisplayName = refreshProfileField(state.DisplayName, member.Displayname)
	}
	if !state.AvatarURL.IsNull() {
		state.AvatarURL = refreshProfileField(state.AvatarURL, string(member.AvatarURL))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userProfileOverrideResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior userProfileOverrideModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	r.applyOverride(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userProfileOverrideResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Matrix has no way to say "inherit the global profile" in an m.room.member
	// event, so writing empty strings here would not restore anything: it would
	// remove the name and render the user as a raw mxid. Same reasoning
	// matrix_user_profile gives for refusing to clear on destroy. Destroy drops
	// only the state tracking. Set the fields to "" and apply first if you really
	// want them gone. See issue #39.
}

func (r *userProfileOverrideResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "|", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <room_id>|<user_id>")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("room_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
