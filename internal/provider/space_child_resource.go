package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var (
	_ resource.Resource                = &spaceChildResource{}
	_ resource.ResourceWithConfigure   = &spaceChildResource{}
	_ resource.ResourceWithImportState = &spaceChildResource{}
	_ resource.ResourceWithModifyPlan  = &spaceChildResource{}
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
	// The content fields are Optional+Computed: leaving one out keeps whatever the
	// space already holds, and UseStateForUnknown stops that value planning as a
	// change on every run. suggested is the one that bit: an m.space.child with no
	// suggested key reads back as false, so an omitted attribute planned
	// false -> null forever and an import never finished. See issue #40.
	keepStr := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	keepBool := []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()}
	keepSet := []planmodifier.Set{setplanmodifier.UseStateForUnknown()}
	resp.Schema = schema.Schema{
		Description: "Links a room or space as a child under a parent space (m.space.child).\n\n" +
			"Removing the link is done by writing empty content, which is the convention for `m.space.child`. A link that ends up with no `via`, `order` or `suggested` is therefore indistinguishable from a removed one, and disappears from state on the next refresh. Always set `via`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Composite ID: <parent_space_id>|<child_room_id>.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"parent_space_id": schema.StringAttribute{Required: true, PlanModifiers: forceNew, Description: "Space room ID that acts as parent."},
			"child_room_id":   schema.StringAttribute{Required: true, PlanModifiers: forceNew, Description: "Room/space ID to include as child."},
			"via": schema.SetAttribute{
				ElementType: types.StringType, Optional: true, Computed: true, PlanModifiers: keepSet,
				Description: "Servers to use when joining the child. The Matrix specification requires this: without it a client cannot resolve the child room. Leave it out and the link keeps whatever the space already holds.",
			},
			"order": schema.StringAttribute{
				Optional: true, Computed: true, PlanModifiers: keepStr,
				Description: "Lexicographic ordering string. Leave it out and the link keeps whatever the space already holds.",
			},
			"suggested": schema.BoolAttribute{
				Optional: true, Computed: true, PlanModifiers: keepBool,
				Description: "Whether clients should suggest the child to users. Leave it out and the link keeps whatever the space already holds.",
			},
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

// modelFromSpaceChild maps event content into the model's three content fields.
// Shared by Read and by the unknown resolution, so both agree on what an absent
// key means.
func modelFromSpaceChild(ctx context.Context, c *event.SpaceChildEventContent, m *spaceChildModel) error {
	if len(c.Via) == 0 {
		m.Via = types.SetNull(types.StringType)
	} else {
		via, d := types.SetValueFrom(ctx, types.StringType, c.Via)
		if d.HasError() {
			return errorFromDiags(d)
		}
		m.Via = via
	}
	if c.Order == "" {
		m.Order = types.StringNull()
	} else {
		m.Order = types.StringValue(c.Order)
	}
	m.Suggested = types.BoolValue(c.Suggested)
	return nil
}

// resolveUnknownSpaceChild fills in only the attributes the plan left unknown,
// from the content just sent.
//
// Anything the plan already decided is kept: Terraform rejects an applied value
// that differs from a known planned value. Being Computed is what makes this
// necessary, and skipping it is what broke Create in issue #29.
func resolveUnknownSpaceChild(ctx context.Context, c *event.SpaceChildEventContent, m *spaceChildModel) error {
	var sent spaceChildModel
	if err := modelFromSpaceChild(ctx, c, &sent); err != nil {
		return err
	}
	if m.Via.IsUnknown() {
		m.Via = sent.Via
	}
	if m.Order.IsUnknown() {
		m.Order = sent.Order
	}
	if m.Suggested.IsUnknown() {
		m.Suggested = sent.Suggested
	}
	return nil
}

// applySpaceChild builds the content, sends it, and resolves what the plan left
// unknown. Mirrors applyPowerLevels.
//
// Resolving from the content just sent rather than a second read is deliberate:
// Matrix stores state event content verbatim, so a read back returns what was
// sent, one round trip later and open to a read-your-writes race.
func (r *spaceChildResource) applySpaceChild(ctx context.Context, m *spaceChildModel, diags *diag.Diagnostics) {
	content, err := buildSpaceChildContent(ctx, m)
	if err != nil {
		diags.AddError("Invalid space_child attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(m.ParentSpaceID.ValueString()), event.StateSpaceChild, m.ChildRoomID.ValueString(), content); err != nil {
		diags.AddError("Failed to set m.space.child", err.Error())
		return
	}
	if err := resolveUnknownSpaceChild(ctx, content, m); err != nil {
		diags.AddError("Failed to map m.space.child into state", err.Error())
	}
}

// spaceChildMissingVia reports whether the link will end up with no via. Pure
// function — no client/network access.
//
// The configuration matters as much as the plan. On a create that declares
// nothing, via is unknown and UseStateForUnknown cannot help, so the plan alone
// cannot tell "not declared" from "will be computed". A null configuration is
// exactly the case worth warning about.
func spaceChildMissingVia(planVia, configVia types.Set) bool {
	if planVia.IsUnknown() {
		return configVia.IsNull()
	}
	return planVia.IsNull() || len(planVia.Elements()) == 0
}

// ModifyPlan warns before an apply produces a link no client can resolve.
func (r *spaceChildResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy plan
	}
	var plan, config spaceChildModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !spaceChildMissingVia(plan.Via, config.Via) {
		return
	}
	resp.Diagnostics.AddAttributeWarning(path.Root("via"), "Space child has no via",
		"The Matrix specification requires `via` on an m.space.child event: without it a client "+
			"cannot resolve the child room, so the link does nothing. A link with no `via`, `order` "+
			"or `suggested` is also indistinguishable from a removed one, because removal is done by "+
			"writing empty content, so it disappears from state on the next refresh.")
}

func (r *spaceChildResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan spaceChildModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.applySpaceChild(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
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
	if err := modelFromSpaceChild(ctx, &c, &state); err != nil {
		resp.Diagnostics.AddError("Failed to map m.space.child into state", err.Error())
		return
	}
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
	r.applySpaceChild(ctx, &plan, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
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
	err := sendState(ctx, r.client, id.RoomID(state.ParentSpaceID.ValueString()), event.StateSpaceChild, state.ChildRoomID.ValueString(), map[string]any{})
	failedDestroy(&resp.Diagnostics, "Failed to remove m.space.child", err)
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
