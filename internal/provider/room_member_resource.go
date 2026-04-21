package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var (
	_ resource.Resource                = &roomMemberResource{}
	_ resource.ResourceWithConfigure   = &roomMemberResource{}
	_ resource.ResourceWithImportState = &roomMemberResource{}
)

type roomMemberResource struct {
	client *Client
}

type roomMemberModel struct {
	ID         types.String `tfsdk:"id"`
	RoomID     types.String `tfsdk:"room_id"`
	UserID     types.String `tfsdk:"user_id"`
	Membership types.String `tfsdk:"membership"`
	Reason     types.String `tfsdk:"reason"`
}

func NewRoomMemberResource() resource.Resource { return &roomMemberResource{} }

func (r *roomMemberResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_room_member"
}

func (r *roomMemberResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "Manages one user's membership in a room from the caller's perspective.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Composite ID: <room_id>|<user_id>.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"room_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew},
			"user_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew},
			"membership": schema.StringAttribute{
				Required:    true,
				Description: "Desired membership: invite | join | leave | ban | knock.",
				Validators: []validator.String{
					oneOfString{"invite", "join", "leave", "ban", "knock"},
				},
			},
			"reason": schema.StringAttribute{Optional: true, Description: "Optional reason sent with the state change."},
		},
	}
}

func (r *roomMemberResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

func applyMembership(ctx context.Context, c *Client, roomID id.RoomID, userID id.UserID, target, reason string) error {
	// Read current membership so the operation is idempotent and we don't fight
	// transitions that already happened (e.g. user accepted an invite).
	var current event.MemberEventContent
	_, _ = getState(ctx, c, roomID, event.StateMember, string(userID), &current)
	cur := string(current.Membership)

	switch target {
	case "invite":
		// invite is satisfied by current ∈ {invite, join}.
		if cur == "invite" || cur == "join" {
			return nil
		}
		_, err := c.MX.InviteUser(ctx, roomID, &mautrix.ReqInviteUser{UserID: userID, Reason: reason})
		return err
	case "leave":
		if cur == "leave" || cur == "ban" {
			return nil
		}
		if userID == c.MX.UserID {
			_, err := c.MX.LeaveRoom(ctx, roomID, &mautrix.ReqLeave{Reason: reason})
			return err
		}
		_, err := c.MX.KickUser(ctx, roomID, &mautrix.ReqKickUser{UserID: userID, Reason: reason})
		return err
	case "ban":
		if cur == "ban" {
			return nil
		}
		_, err := c.MX.BanUser(ctx, roomID, &mautrix.ReqBanUser{UserID: userID, Reason: reason})
		return err
	case "join":
		// Only the target user can join themselves. If they're already joined, that's fine.
		// Otherwise we can't force it — surface a clear error.
		if cur == "join" {
			return nil
		}
		if userID == c.MX.UserID {
			_, err := c.MX.JoinRoomByID(ctx, roomID)
			return err
		}
		return fmt.Errorf("cannot force %s to join: membership=join can only be set by the target user (current membership=%q)", userID, cur)
	case "knock":
		if cur == "knock" || cur == "invite" || cur == "join" {
			return nil
		}
		content := map[string]any{"membership": "knock"}
		if reason != "" {
			content["reason"] = reason
		}
		_, err := c.MX.SendStateEvent(ctx, roomID, event.StateMember, string(userID), content)
		return err
	default:
		return fmt.Errorf("unsupported membership transition %q", target)
	}
}

func (r *roomMemberResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan roomMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := applyMembership(ctx, r.client, id.RoomID(plan.RoomID.ValueString()), id.UserID(plan.UserID.ValueString()),
		plan.Membership.ValueString(), plan.Reason.ValueString()); err != nil {
		resp.Diagnostics.AddError("Failed to apply membership", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.RoomID.ValueString() + "|" + plan.UserID.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomMemberResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roomMemberModel
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
	state.Membership = types.StringValue(string(member.Membership))
	// `reason` is a transition parameter (attached to the invite/kick/ban event), not
	// part of the settled member state. Leave whatever the last TF apply recorded.
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *roomMemberResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior roomMemberModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	if err := applyMembership(ctx, r.client, id.RoomID(plan.RoomID.ValueString()), id.UserID(plan.UserID.ValueString()),
		plan.Membership.ValueString(), plan.Reason.ValueString()); err != nil {
		resp.Diagnostics.AddError("Failed to update membership", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomMemberResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roomMemberModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Best-effort: kick on destroy. If the user is already gone we swallow the error.
	if err := applyMembership(ctx, r.client, id.RoomID(state.RoomID.ValueString()), id.UserID(state.UserID.ValueString()), "leave", ""); err != nil {
		resp.Diagnostics.AddWarning("Membership cleanup on destroy failed", err.Error())
	}
}

func (r *roomMemberResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "|", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <room_id>|<user_id>")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("room_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("user_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
