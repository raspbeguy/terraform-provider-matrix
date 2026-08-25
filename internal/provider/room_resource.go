package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/event"
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

// replaceIfWasKnownStr forces replacement only when the prior state holds a
// value. RequiresReplace fires on null -> value too, and after an import every
// unread attribute is null, so a configured value would destroy the room the
// import was meant to adopt. See issue #32.
//
// The framework already skips create, destroy, and the no-change case before it
// calls this, so only a known-to-known change reaches it.
func replaceIfWasKnownStr() planmodifier.String {
	const desc = "Forces replacement when the value changes, but not when the prior state is null."
	return stringplanmodifier.RequiresReplaceIf(
		func(_ context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
			resp.RequiresReplace = !req.StateValue.IsNull()
		}, desc, desc)
}

func (r *roomResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	desc := "A Matrix room."
	if r.isSpace {
		desc = "A Matrix space (a room with `creation_content.type = m.space`). At creation, applies Element-style defaults that lock messages to admins (`events_default = 100`) and let moderators invite (`invite = 50`), atomically with the /createRoom call. Override or relax these via a `matrix_room_power_levels` resource pointing at this space."
	}
	// Create-only attributes are Optional+Computed so that Read can populate
	// them without a config that omits them diffing forever, and they force
	// replacement only from a known prior value. A null prior state means an
	// import, or an attribute added to the config of a room that already exists,
	// and replacing there destroys the room the import was meant to adopt.
	// See issue #32.
	createOnlyStr := []planmodifier.String{replaceIfWasKnownStr(), stringplanmodifier.UseStateForUnknown()}
	keepStr := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	keepBool := []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()}
	keepSet := []planmodifier.Set{setplanmodifier.UseStateForUnknown()}

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
			Computed:      true,
			Description:   "Creation preset: private_chat | trusted_private_chat | public_chat. Applies at creation only, and no endpoint reports it back, so an imported room records it on the first apply.",
			Validators:    []validator.String{oneOfString{"private_chat", "trusted_private_chat", "public_chat"}},
			PlanModifiers: createOnlyStr,
		},
		"visibility": schema.StringAttribute{
			Optional:      true,
			Computed:      true,
			Description:   "Directory visibility: public | private. Updatable after creation, and re-read on every refresh, so a change made outside Terraform shows as drift. Synapse denies room directory publication by default, through its room_list_publication_rules, so a room asked to be `public` commonly stays `private`. The provider warns, and because the directory keeps its own value, every later plan shows the difference. Declare this attribute only against a homeserver known to allow publication; leave it out and the room adopts whatever the homeserver decides.",
			Validators:    []validator.String{oneOfString{"public", "private"}},
			PlanModifiers: keepStr,
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
			Computed:      true,
			Description:   "Room version (e.g. \"11\"). If unset, reflects the version the homeserver chose. A room's version never changes; an upgrade creates a new room.",
			PlanModifiers: createOnlyStr,
		},
		"room_alias_name": schema.StringAttribute{
			Optional:      true,
			Computed:      true,
			Description:   "Localpart of the canonical alias to set at creation. On import, adopted from the room's canonical alias when it is local to this homeserver.",
			PlanModifiers: createOnlyStr,
		},
		"initial_invites": schema.SetAttribute{
			ElementType:   types.StringType,
			Optional:      true,
			Computed:      true,
			Description:   "User IDs to invite during room creation. Subsequent changes are ignored — use matrix_room_member.",
			PlanModifiers: keepSet,
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
			Computed:      true,
			Description:   "If true, enable end-to-end encryption at creation time. Cannot be disabled once set. On import, adopted from m.room.encryption.",
			PlanModifiers: keepBool,
		}
		attrs["is_direct"] = schema.BoolAttribute{
			Optional:      true,
			Computed:      true,
			Description:   "Mark the room as a direct chat. Applies at creation only, and no endpoint reports it back, so an imported room records it on the first apply.",
			PlanModifiers: keepBool,
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
		if resp.Diagnostics.HasError() {
			return
		}
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
	readRoomOnlyState(ctx, r.client, roomID, &plan)
	if resp.Diagnostics.HasError() {
		// readRoomLikeState returns at the first failure, which skips the pass
		// that resolves the Computed attributes. Writing state now would report
		// an unknown value on top of the real error.
		return
	}
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
		refreshRoomVisibility(ctx, r.client, roomID, &state.baseRoomModel)
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
	refreshRoomVisibility(ctx, r.client, roomID, &state.baseRoomModel)
	readRoomOnlyState(ctx, r.client, roomID, &state)
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
		if resp.Diagnostics.HasError() {
			return
		}
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
	readRoomOnlyState(ctx, r.client, roomID, &plan)
	if resp.Diagnostics.HasError() {
		// readRoomLikeState returns at the first failure, which skips the pass
		// that resolves the Computed attributes. Writing state now would report
		// an unknown value on top of the real error.
		return
	}
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
	err := leaveRoom(ctx, r.client, id.RoomID(state.ID.ValueString()))
	failedDestroy(&resp.Diagnostics, "Leaving room on destroy failed", err)
}

// wrongResourceTypeDetail reports the mismatch when a room ID is imported into
// the wrong resource, or "" when the two agree. A space is a room with
// creation_content.type = m.space, so both resources accept any room ID and
// would then fight it on every plan.
func wrongResourceTypeDetail(roomID string, roomType event.RoomType, isSpace bool) string {
	roomIsSpace := roomType == event.RoomTypeSpace
	if roomIsSpace == isSpace {
		return ""
	}
	have, want := "matrix_room", "matrix_space"
	if roomIsSpace {
		have, want = "matrix_space", "matrix_room"
	}
	return roomID + " is a " + have + ", not a " + want + ". Import it as " + have + " instead."
}

func (r *roomResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if r.client == nil {
		resp.Diagnostics.AddError("Provider not configured",
			"The Matrix client is not available. This is a bug in the provider.")
		return
	}
	// A space is a room with creation_content.type = m.space, so a bare
	// passthrough lets either resource adopt either kind and then fight it on
	// every plan. Check before anything lands in state.
	create, found, err := getCreateContent(ctx, r.client, id.RoomID(req.ID))
	switch {
	case err != nil:
		resp.Diagnostics.AddError("Failed to read m.room.create", err.Error())
		return
	case !found:
		resp.Diagnostics.AddError("Room not found",
			"No m.room.create event is readable for "+req.ID+". Check the room ID, and that this account is in the room.")
		return
	}
	if detail := wrongResourceTypeDetail(req.ID, create.Type, r.isSpace); detail != "" {
		resp.Diagnostics.AddError("Wrong resource type for this room", detail)
		return
	}
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
