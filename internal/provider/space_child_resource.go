package provider

import (
	"context"
	"strings"

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
	_ resource.Resource                = &spaceChildResource{}
	_ resource.ResourceWithConfigure   = &spaceChildResource{}
	_ resource.ResourceWithImportState = &spaceChildResource{}
)

type spaceChildResource struct {
	client *Client
}

type spaceChildModel struct {
	ID            types.String `tfsdk:"id"`
	ParentSpaceID types.String `tfsdk:"parent_space_id"`
	ChildRoomID   types.String `tfsdk:"child_room_id"`
	Via           types.Set    `tfsdk:"via"`
	Order         types.String `tfsdk:"order"`
	Suggested     types.Bool   `tfsdk:"suggested"`
}

func NewSpaceChildResource() resource.Resource { return &spaceChildResource{} }

func (r *spaceChildResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_space_child"
}

func (r *spaceChildResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "Links a room or space as a child under a parent space (m.space.child).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Composite ID: <parent_space_id>|<child_room_id>.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"parent_space_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew, Description: "Space room ID that acts as parent."},
			"child_room_id":   schema.StringAttribute{Required: true, PlanModifiers: forceNew, Description: "Room/space ID to include as child."},
			"via":             schema.SetAttribute{ElementType: types.StringType, Optional: true, Description: "Servers to use when joining the child."},
			"order":           schema.StringAttribute{Optional: true, Description: "Lexicographic ordering string."},
			"suggested":       schema.BoolAttribute{Optional: true, Description: "Whether clients should suggest the child to users."},
		},
	}
}

func (r *spaceChildResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

func buildSpaceChildContent(ctx context.Context, m *spaceChildModel) (*event.SpaceChildEventContent, error) {
	c := &event.SpaceChildEventContent{
		Order:     m.Order.ValueString(),
		Suggested: m.Suggested.ValueBool(),
	}
	if !m.Via.IsNull() && !m.Via.IsUnknown() {
		var via []string
		diags := m.Via.ElementsAs(ctx, &via, false)
		if diags.HasError() {
			return nil, errorFromDiags(diags)
		}
		c.Via = via
	}
	return c, nil
}

func (r *spaceChildResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan spaceChildModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	content, err := buildSpaceChildContent(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid space_child attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(plan.ParentSpaceID.ValueString()), event.StateSpaceChild, plan.ChildRoomID.ValueString(), content); err != nil {
		resp.Diagnostics.AddError("Failed to set m.space.child", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.ParentSpaceID.ValueString() + "|" + plan.ChildRoomID.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *spaceChildResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state spaceChildModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var c event.SpaceChildEventContent
	found, err := getState(ctx, r.client, id.RoomID(state.ParentSpaceID.ValueString()), event.StateSpaceChild, state.ChildRoomID.ValueString(), &c)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read m.space.child", err.Error())
		return
	}
	if !found || (len(c.Via) == 0 && c.Order == "" && !c.Suggested) {
		// Empty content == edge removed. Drop from state.
		resp.State.RemoveResource(ctx)
		return
	}
	if len(c.Via) == 0 {
		state.Via = types.SetNull(types.StringType)
	} else {
		via, d := types.SetValueFrom(ctx, types.StringType, c.Via)
		resp.Diagnostics.Append(d...)
		state.Via = via
	}
	if c.Order == "" {
		state.Order = types.StringNull()
	} else {
		state.Order = types.StringValue(c.Order)
	}
	state.Suggested = types.BoolValue(c.Suggested)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *spaceChildResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior spaceChildModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	content, err := buildSpaceChildContent(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid space_child attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(plan.ParentSpaceID.ValueString()), event.StateSpaceChild, plan.ChildRoomID.ValueString(), content); err != nil {
		resp.Diagnostics.AddError("Failed to update m.space.child", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *spaceChildResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state spaceChildModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(state.ParentSpaceID.ValueString()), event.StateSpaceChild, state.ChildRoomID.ValueString(), map[string]any{}); err != nil {
		resp.Diagnostics.AddError("Failed to remove m.space.child", err.Error())
	}
}

func (r *spaceChildResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "|", 2)
	if len(parts) != 2 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <parent_space_id>|<child_room_id>")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("parent_space_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("child_room_id"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
