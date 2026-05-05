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
			"Destroy semantics: removes display_name and avatar_url from the m.room.member event (revealing the global profile again) while preserving the user's membership.",
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
				Description: "Per-room display name. If null, the user's global display name is used.",
			},
			"avatar_url": schema.StringAttribute{
				Optional:    true,
				Description: "Per-room avatar mxc:// URI. If null, the user's global avatar is used.",
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

// applyOverride reads the current m.room.member event, overlays the provided
// displayname/avatar (clearing them when the model field is null), and re-sends
// the event. Preserves membership and other fields the spec defines on
// m.room.member that the provider doesn't manage.
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

	if m.DisplayName.IsNull() {
		current.Displayname = ""
	} else {
		current.Displayname = m.DisplayName.ValueString()
	}
	if m.AvatarURL.IsNull() {
		current.AvatarURL = ""
	} else {
		uri, perr := id.ParseContentURI(m.AvatarURL.ValueString())
		if perr != nil {
			diags.AddAttributeError(path.Root("avatar_url"), "Invalid mxc URI", perr.Error())
			return
		}
		current.AvatarURL = uri.CUString()
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
		if member.Displayname == "" {
			state.DisplayName = types.StringNull()
		} else {
			state.DisplayName = types.StringValue(member.Displayname)
		}
	}
	if !state.AvatarURL.IsNull() {
		if member.AvatarURL == "" {
			state.AvatarURL = types.StringNull()
		} else {
			state.AvatarURL = types.StringValue(string(member.AvatarURL))
		}
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

func (r *userProfileOverrideResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state userProfileOverrideModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Wipe the override fields but keep membership intact.
	clear := userProfileOverrideModel{
		RoomID:      state.RoomID,
		UserID:      state.UserID,
		DisplayName: types.StringNull(),
		AvatarURL:   types.StringNull(),
	}
	var diags diag.Diagnostics
	r.applyOverride(ctx, &clear, &diags)
	if diags.HasError() {
		resp.Diagnostics.AddWarning("Failed to clear per-room profile override on destroy",
			"Resource removed from state anyway. Server message: "+diagsError(diags).Error())
	}
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
