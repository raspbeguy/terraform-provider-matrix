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
		Description: "Manages one user's membership in a room from the caller's perspective.\n\n" +
			"This resource is declarative: as long as the HCL says `membership = \"invite\"`, the provider keeps the room reconciled to that intent. " +
			"If the user accepts the invite and later leaves the room, the next `terraform apply` will re-invite them, since `\"leave\"` no longer matches the declared `\"invite\"`. " +
			"To stop reconciling, either `terraform state rm` the resource (drops it from state, leaves the server alone) or add a `lifecycle { ignore_changes = [membership] }` block (initial invite still fires; subsequent leaves are ignored).",
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
				Description: "Desired membership: invite | join | leave | ban | knock. Read as \"at least\": if you declare `invite` and the user later accepts (membership becomes `join`), no drift is reported because `join` semantically satisfies `invite`. Same for `leave` ⊆ `ban`, and `knock` ⊆ `invite`/`join`.",
				Validators: []validator.String{
					oneOfString{"invite", "join", "leave", "ban", "knock"},
				},
				PlanModifiers: []planmodifier.String{membershipSatisfiedByModifier{}},
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

// membershipSatisfiedBy reports whether the current membership semantically
// satisfies the desired one — i.e. whether applyMembership would be a no-op
// if called with `wanted` and the server already has `current`.
//
// The same table drives both the apply-time idempotency check and the
// plan-time drift suppression, so the two stay consistent.
func membershipSatisfiedBy(wanted, current string) bool {
	switch wanted {
	case "invite":
		return current == "invite" || current == "join"
	case "knock":
		return current == "knock" || current == "invite" || current == "join"
	case "leave":
		return current == "leave" || current == "ban"
	case "ban":
		return current == "ban"
	case "join":
		return current == "join"
	}
	return false
}

func applyMembership(ctx context.Context, c *Client, roomID id.RoomID, userID id.UserID, target, reason string) error {
	// Read current membership so the operation is idempotent and we don't fight
	// transitions that already happened (e.g. user accepted an invite).
	var current event.MemberEventContent
	_, _ = getState(ctx, c, roomID, event.StateMember, string(userID), &current)
	cur := string(current.Membership)

	if membershipSatisfiedBy(target, cur) {
		return nil
	}

	switch target {
	case "invite":
		_, err := c.MX.InviteUser(ctx, roomID, &mautrix.ReqInviteUser{UserID: userID, Reason: reason})
		return err
	case "leave":
		if userID == c.MX.UserID {
			_, err := c.MX.LeaveRoom(ctx, roomID, &mautrix.ReqLeave{Reason: reason})
			return err
		}
		_, err := c.MX.KickUser(ctx, roomID, &mautrix.ReqKickUser{UserID: userID, Reason: reason})
		return err
	case "ban":
		_, err := c.MX.BanUser(ctx, roomID, &mautrix.ReqBanUser{UserID: userID, Reason: reason})
		return err
	case "join":
		// Only the target user can join themselves. The "already joined" case is
		// handled by the satisfaction check above; this branch fires when the
		// caller is asking us to join someone else, which we can't do.
		if userID == c.MX.UserID {
			_, err := c.MX.JoinRoomByID(ctx, roomID)
			return err
		}
		return fmt.Errorf("cannot force %s to join: membership=join can only be set by the target user (current membership=%q)", userID, cur)
	case "knock":
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
	// applyMembership already returns nil when the membership is leave or ban, so
	// an error here means the homeserver refused a change that still has to
	// happen. Kicking anyone but the caller needs the kick level, 50 by default.
	err := applyMembership(ctx, r.client, id.RoomID(state.RoomID.ValueString()), id.UserID(state.UserID.ValueString()), "leave", "")
	failedDestroy(&resp.Diagnostics, "Membership cleanup on destroy failed", err)
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
