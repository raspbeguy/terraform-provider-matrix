package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/id"
)

var (
	_ resource.Resource                = &roomAliasResource{}
	_ resource.ResourceWithConfigure   = &roomAliasResource{}
	_ resource.ResourceWithImportState = &roomAliasResource{}
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
		Description: "Maps an alias (#name:server) to a room in the homeserver's directory.",
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
		resp.State.RemoveResource(ctx)
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
	if _, err := r.client.MX.DeleteAlias(ctx, id.RoomAlias(prior.Alias.ValueString())); err != nil {
		resp.Diagnostics.AddError("Failed to update alias (delete step)", err.Error())
		return
	}
	if _, err := r.client.MX.CreateAlias(ctx, id.RoomAlias(plan.Alias.ValueString()), id.RoomID(plan.RoomID.ValueString())); err != nil {
		resp.Diagnostics.AddError("Failed to update alias (create step)", err.Error())
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
	if _, err := r.client.MX.DeleteAlias(ctx, id.RoomAlias(state.Alias.ValueString())); err != nil {
		resp.Diagnostics.AddWarning("Alias delete failed", err.Error())
	}
}

func (r *roomAliasResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("alias"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
