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

	"maunium.net/go/mautrix/id"
)

var (
	_ resource.Resource                   = &roomResource{}
	_ resource.ResourceWithConfigure      = &roomResource{}
	_ resource.ResourceWithImportState    = &roomResource{}
	_ resource.ResourceWithValidateConfig = &roomResource{}
)

type roomResource struct {
	client  *Client
	isSpace bool
}

func NewRoomResource() resource.Resource  { return &roomResource{} }
func NewSpaceResource() resource.Resource { return &roomResource{isSpace: true} }

func (r *roomResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	if r.isSpace {
		resp.TypeName = req.ProviderTypeName + "_space"
	} else {
		resp.TypeName = req.ProviderTypeName + "_room"
	}
}

func (r *roomResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	desc := "A Matrix room."
	if r.isSpace {
		desc = "A Matrix space (a room with `creation_content.type = m.space`). At creation, applies Element-style defaults that lock messages to admins (`events_default = 100`) and let moderators invite (`invite = 50`), atomically with the /createRoom call. Override or relax these via a `matrix_room_power_levels` resource pointing at this space."
	}
	forceNewStr := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: desc,
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Matrix room ID (!abc:server).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name":       schema.StringAttribute{Optional: true, Description: "Room name (m.room.name)."},
			"topic":      schema.StringAttribute{Optional: true, Description: "Room topic (m.room.topic)."},
			"avatar_url": schema.StringAttribute{Optional: true, Description: "Avatar mxc:// URI (m.room.avatar)."},
			"preset": schema.StringAttribute{
				Optional:      true,
				Description:   "Creation preset: private_chat | trusted_private_chat | public_chat.",
				PlanModifiers: forceNewStr,
			},
			"visibility": schema.StringAttribute{
				Optional:      true,
				Description:   "Directory visibility: public | private.",
				PlanModifiers: forceNewStr,
			},
			"history_visibility": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Controls who can read the timeline: joined | invited | shared | world_readable. If unset, reflects the homeserver's default. Updatable after creation.",
				Validators: []validator.String{
					oneOfString{"joined", "invited", "shared", "world_readable"},
				},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"room_version": schema.StringAttribute{
				Optional:      true,
				Description:   "Room version (e.g. \"11\").",
				PlanModifiers: forceNewStr,
			},
			"room_alias_name": schema.StringAttribute{
				Optional:      true,
				Description:   "Localpart of the canonical alias to set at creation.",
				PlanModifiers: forceNewStr,
			},
			"encryption_enabled": schema.BoolAttribute{
				Optional:      true,
				Description:   "If true, enable end-to-end encryption at creation time. Cannot be disabled once set.",
				PlanModifiers: []planmodifier.Bool{},
			},
			"initial_invites": schema.SetAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Description: "User IDs to invite during room creation. Subsequent changes are ignored — use matrix_room_member.",
			},
			"is_direct": schema.BoolAttribute{
				Optional:      true,
				Description:   "Mark the room as a direct chat.",
				PlanModifiers: []planmodifier.Bool{},
			},
			"canonical_alias": schema.StringAttribute{
				Computed:    true,
				Description: "Canonical alias currently set on the room.",
			},
		},
	}
}

func (r *roomResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

func (r *roomResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan roomLikeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	roomID := createRoomLike(ctx, r.client, &plan, r.isSpace, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(string(roomID))

	// Refresh computed attrs (canonical_alias) from state immediately.
	readRoomLikeState(ctx, r.client, roomID, &plan, &resp.Diagnostics)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roomLikeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	roomID := id.RoomID(state.ID.ValueString())
	readRoomLikeState(ctx, r.client, roomID, &state, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *roomResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior roomLikeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	roomID := id.RoomID(prior.ID.ValueString())
	plan.ID = prior.ID

	syncMutableStateFromModel(ctx, r.client, roomID, &plan, &prior, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	readRoomLikeState(ctx, r.client, roomID, &plan, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roomLikeModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := leaveRoomBestEffort(ctx, r.client, id.RoomID(state.ID.ValueString())); err != nil {
		// Best-effort: log a warning but don't fail destroy — the user may already have left.
		resp.Diagnostics.AddWarning("Leaving room on destroy failed",
			"Removed resource from state anyway. Server message: "+err.Error())
	}
}

func (r *roomResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

func (r *roomResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	if !r.isSpace {
		return
	}
	var m roomLikeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(validateSpaceModel(m)...)
}

// validateSpaceModel rejects attributes that are incoherent on spaces.
// Pure function to keep the logic unit-testable without framework plumbing.
func validateSpaceModel(m roomLikeModel) diag.Diagnostics {
	var diags diag.Diagnostics
	if m.Encryption.ValueBool() {
		diags.AddAttributeError(path.Root("encryption_enabled"),
			"encryption_enabled not valid on matrix_space",
			"Encrypted spaces are not coherently supported by clients. Encrypt individual rooms under the space instead.")
	}
	if m.IsDirect.ValueBool() {
		diags.AddAttributeError(path.Root("is_direct"),
			"is_direct not valid on matrix_space",
			"Spaces cannot be direct chats. Drop this attribute or move it onto a matrix_room.")
	}
	return diags
}
