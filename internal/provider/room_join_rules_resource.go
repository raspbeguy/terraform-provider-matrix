package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var (
	_ resource.Resource                   = &joinRulesResource{}
	_ resource.ResourceWithConfigure      = &joinRulesResource{}
	_ resource.ResourceWithImportState    = &joinRulesResource{}
	_ resource.ResourceWithValidateConfig = &joinRulesResource{}
)

type joinRulesResource struct{ client *Client }

type joinRulesModel struct {
	ID         types.String `tfsdk:"id"`
	RoomID     types.String `tfsdk:"room_id"`
	JoinRule   types.String `tfsdk:"join_rule"`
	AllowRooms types.Set    `tfsdk:"allow_rooms"`
}

func NewRoomJoinRulesResource() resource.Resource { return &joinRulesResource{} }

func (r *joinRulesResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_room_join_rules"
}

func (r *joinRulesResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the m.room.join_rules state event for a room or space. Use `restricted` with `allow_rooms` to gate joinability on membership of another space.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Equal to room_id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"room_id": schema.StringAttribute{
				Required:      true,
				Description:   "ID of the room or space to manage join rules for.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"join_rule": schema.StringAttribute{
				Required:    true,
				Description: "One of: public | invite | knock | restricted | knock_restricted.",
				Validators: []validator.String{
					oneOfString{"public", "invite", "knock", "restricted", "knock_restricted"},
				},
			},
			"allow_rooms": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "For restricted/knock_restricted: room IDs whose members are allowed to join. Typically a matrix_space.id, but any room ID is permitted by the spec.",
			},
		},
	}
}

func (r *joinRulesResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

func (r *joinRulesResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m joinRulesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validateJoinRulesModel(m)...)
}

// validateJoinRulesModel checks consistency between join_rule and allow_rooms.
// Pure function to keep the logic unit-testable without framework plumbing.
func validateJoinRulesModel(m joinRulesModel) diag.Diagnostics {
	var diags diag.Diagnostics
	jr := m.JoinRule.ValueString()
	restricted := jr == "restricted" || jr == "knock_restricted"
	hasAllow := !m.AllowRooms.IsNull() && !m.AllowRooms.IsUnknown() && len(m.AllowRooms.Elements()) > 0
	switch {
	case restricted && !hasAllow:
		diags.AddAttributeError(path.Root("allow_rooms"),
			"allow_rooms required", "join_rule="+jr+" must specify at least one entry in allow_rooms.")
	case !restricted && hasAllow:
		diags.AddAttributeError(path.Root("allow_rooms"),
			"allow_rooms only valid with restricted join rules",
			"allow_rooms is only meaningful when join_rule is restricted or knock_restricted.")
	}
	return diags
}

func buildJoinRulesContent(ctx context.Context, m *joinRulesModel) (*event.JoinRulesEventContent, error) {
	c := &event.JoinRulesEventContent{JoinRule: event.JoinRule(m.JoinRule.ValueString())}
	if !m.AllowRooms.IsNull() && !m.AllowRooms.IsUnknown() {
		var ids []string
		if d := m.AllowRooms.ElementsAs(ctx, &ids, false); d.HasError() {
			return nil, errorFromDiags(d)
		}
		for _, rid := range ids {
			c.Allow = append(c.Allow, event.JoinRuleAllow{
				Type:   event.JoinRuleAllowRoomMembership,
				RoomID: id.RoomID(rid),
			})
		}
	}
	return c, nil
}

func (r *joinRulesResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan joinRulesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	content, err := buildJoinRulesContent(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid join_rules attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(plan.RoomID.ValueString()), event.StateJoinRules, "", content); err != nil {
		resp.Diagnostics.AddError("Failed to set m.room.join_rules", err.Error())
		return
	}
	plan.ID = plan.RoomID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *joinRulesResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state joinRulesModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var c event.JoinRulesEventContent
	found, err := getState(ctx, r.client, id.RoomID(state.RoomID.ValueString()), event.StateJoinRules, "", &c)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read m.room.join_rules", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	state.JoinRule = types.StringValue(string(c.JoinRule))
	if len(c.Allow) == 0 {
		state.AllowRooms = types.SetNull(types.StringType)
	} else {
		// TODO: the Matrix spec currently defines exactly one allow-entry type
		// (m.room_membership). If that ever gains siblings, silently dropping them
		// here produces endless drift. Revisit when a new type appears: either
		// expose raw allow entries, or fail loudly on unknown types.
		rooms := make([]string, 0, len(c.Allow))
		for _, a := range c.Allow {
			if a.Type == event.JoinRuleAllowRoomMembership {
				rooms = append(rooms, string(a.RoomID))
			} else {
				resp.Diagnostics.AddWarning("Unknown join_rules allow entry type",
					"Read found an allow entry with type="+string(a.Type)+
						" which is not known to this provider version. It will be dropped from Terraform state; refreshing may show unexpected drift.")
			}
		}
		val, d := types.SetValueFrom(ctx, types.StringType, rooms)
		resp.Diagnostics.Append(d...)
		state.AllowRooms = val
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *joinRulesResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior joinRulesModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	content, err := buildJoinRulesContent(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid join_rules attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(plan.RoomID.ValueString()), event.StateJoinRules, "", content); err != nil {
		resp.Diagnostics.AddError("Failed to update m.room.join_rules", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *joinRulesResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// join_rules can't be truly deleted; destroy drops only the state tracking.
}

func (r *joinRulesResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("room_id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
