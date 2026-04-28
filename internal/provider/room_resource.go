package provider

import (
	"context"

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
	_ resource.Resource                = &roomResource{}
	_ resource.ResourceWithConfigure   = &roomResource{}
	_ resource.ResourceWithImportState = &roomResource{}
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

	attrs := map[string]schema.Attribute{
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
		"initial_invites": schema.SetAttribute{
			ElementType: types.StringType,
			Optional:    true,
			Description: "User IDs to invite during room creation. Subsequent changes are ignored — use matrix_room_member.",
		},
		"canonical_alias": schema.StringAttribute{
			Computed:    true,
			Description: "Canonical alias currently set on the room.",
		},
	}

	// Room-only attributes that are nonsensical for spaces.
	if !r.isSpace {
		attrs["encryption_enabled"] = schema.BoolAttribute{
			Optional:      true,
			Description:   "If true, enable end-to-end encryption at creation time. Cannot be disabled once set.",
			PlanModifiers: []planmodifier.Bool{},
		}
		attrs["is_direct"] = schema.BoolAttribute{
			Optional:      true,
			Description:   "Mark the room as a direct chat.",
			PlanModifiers: []planmodifier.Bool{},
		}
	}

	resp.Schema = schema.Schema{
		Description: desc,
		Attributes:  attrs,
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
	if r.isSpace {
		var plan spaceModel
		resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
		if resp.Diagnostics.HasError() {
			return
		}
		roomID := createRoomLike(ctx, r.client, &plan.baseRoomModel, false, false, true, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		plan.ID = types.StringValue(string(roomID))
		readRoomLikeState(ctx, r.client, roomID, &plan.baseRoomModel, &resp.Diagnostics)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	var plan roomModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	roomID := createRoomLike(ctx, r.client, &plan.baseRoomModel,
		plan.Encryption.ValueBool(), plan.IsDirect.ValueBool(), false, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue(string(roomID))
	readRoomLikeState(ctx, r.client, roomID, &plan.baseRoomModel, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	if r.isSpace {
		var state spaceModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		roomID := id.RoomID(state.ID.ValueString())
		readRoomLikeState(ctx, r.client, roomID, &state.baseRoomModel, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	var state roomModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	roomID := id.RoomID(state.ID.ValueString())
	readRoomLikeState(ctx, r.client, roomID, &state.baseRoomModel, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *roomResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	if r.isSpace {
		var plan, prior spaceModel
		resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
		resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
		if resp.Diagnostics.HasError() {
			return
		}
		roomID := id.RoomID(prior.ID.ValueString())
		plan.ID = prior.ID
		syncMutableStateFromModel(ctx, r.client, roomID, &plan.baseRoomModel, &prior.baseRoomModel, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		readRoomLikeState(ctx, r.client, roomID, &plan.baseRoomModel, &resp.Diagnostics)
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	var plan, prior roomModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	roomID := id.RoomID(prior.ID.ValueString())
	plan.ID = prior.ID
	syncMutableStateFromModel(ctx, r.client, roomID, &plan.baseRoomModel, &prior.baseRoomModel, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	readRoomLikeState(ctx, r.client, roomID, &plan.baseRoomModel, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Both variants only need the room ID; read into the base model regardless.
	var state baseRoomModel
	if r.isSpace {
		var s spaceModel
		resp.Diagnostics.Append(req.State.Get(ctx, &s)...)
		state = s.baseRoomModel
	} else {
		var s roomModel
		resp.Diagnostics.Append(req.State.Get(ctx, &s)...)
		state = s.baseRoomModel
	}
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
