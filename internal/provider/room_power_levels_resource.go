package provider

import (
	"context"

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
	_ resource.Resource                = &powerLevelsResource{}
	_ resource.ResourceWithConfigure   = &powerLevelsResource{}
	_ resource.ResourceWithImportState = &powerLevelsResource{}
)

type powerLevelsResource struct{ client *Client }

type powerLevelsModel struct {
	ID            types.String `tfsdk:"id"`
	RoomID        types.String `tfsdk:"room_id"`
	UsersDefault  types.Int64  `tfsdk:"users_default"`
	EventsDefault types.Int64  `tfsdk:"events_default"`
	StateDefault  types.Int64  `tfsdk:"state_default"`
	Ban           types.Int64  `tfsdk:"ban"`
	Kick          types.Int64  `tfsdk:"kick"`
	Invite        types.Int64  `tfsdk:"invite"`
	Redact        types.Int64  `tfsdk:"redact"`
	Users         types.Map    `tfsdk:"users"`
	Events        types.Map    `tfsdk:"events"`
	NotifyRoom    types.Int64  `tfsdk:"notify_room"`
}

func NewRoomPowerLevelsResource() resource.Resource { return &powerLevelsResource{} }

func (r *powerLevelsResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_room_power_levels"
}

func (r *powerLevelsResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the m.room.power_levels state event for a single room. Works on any room-like entity, including spaces — point `room_id` at a matrix_space.id to tune its permissions (e.g. to unlock messages in a space, set `events_default = 0`).",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "Equal to room_id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"room_id": schema.StringAttribute{
				Required: true, Description: "ID of the room or space to manage power levels for.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"users_default":  schema.Int64Attribute{Optional: true, Description: "Default level for users not listed in `users`."},
			"events_default": schema.Int64Attribute{Optional: true, Description: "Default level to send message events."},
			"state_default":  schema.Int64Attribute{Optional: true, Description: "Default level to send state events."},
			"ban":            schema.Int64Attribute{Optional: true},
			"kick":           schema.Int64Attribute{Optional: true},
			"invite":         schema.Int64Attribute{Optional: true},
			"redact":         schema.Int64Attribute{Optional: true},
			"users":          schema.MapAttribute{Optional: true, ElementType: types.Int64Type, Description: "Per-user overrides by mxid."},
			"events":         schema.MapAttribute{Optional: true, ElementType: types.Int64Type, Description: "Per-event-type overrides."},
			"notify_room":    schema.Int64Attribute{Optional: true, Description: "Power level required for @room notifications (notifications.room)."},
		},
	}
}

func (r *powerLevelsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

func powerLevelsFromModel(ctx context.Context, m *powerLevelsModel) (*event.PowerLevelsEventContent, error) {
	pl := &event.PowerLevelsEventContent{}
	if !m.UsersDefault.IsNull() {
		pl.UsersDefault = int(m.UsersDefault.ValueInt64())
	}
	if !m.EventsDefault.IsNull() {
		pl.EventsDefault = int(m.EventsDefault.ValueInt64())
	}
	if !m.StateDefault.IsNull() {
		v := int(m.StateDefault.ValueInt64())
		pl.StateDefaultPtr = &v
	}
	if !m.Ban.IsNull() {
		v := int(m.Ban.ValueInt64())
		pl.BanPtr = &v
	}
	if !m.Kick.IsNull() {
		v := int(m.Kick.ValueInt64())
		pl.KickPtr = &v
	}
	if !m.Invite.IsNull() {
		v := int(m.Invite.ValueInt64())
		pl.InvitePtr = &v
	}
	if !m.Redact.IsNull() {
		v := int(m.Redact.ValueInt64())
		pl.RedactPtr = &v
	}
	if !m.Users.IsNull() && !m.Users.IsUnknown() {
		raw := map[string]int64{}
		if diags := m.Users.ElementsAs(ctx, &raw, false); diags.HasError() {
			return nil, errorFromDiags(diags)
		}
		pl.Users = make(map[id.UserID]int, len(raw))
		for k, v := range raw {
			pl.Users[id.UserID(k)] = int(v)
		}
	}
	if !m.Events.IsNull() && !m.Events.IsUnknown() {
		raw := map[string]int64{}
		if diags := m.Events.ElementsAs(ctx, &raw, false); diags.HasError() {
			return nil, errorFromDiags(diags)
		}
		pl.Events = make(map[string]int, len(raw))
		for k, v := range raw {
			pl.Events[k] = int(v)
		}
	}
	if !m.NotifyRoom.IsNull() {
		v := int(m.NotifyRoom.ValueInt64())
		pl.Notifications = &event.NotificationPowerLevels{RoomPtr: &v}
	}
	return pl, nil
}

func modelFromPowerLevels(ctx context.Context, pl *event.PowerLevelsEventContent, m *powerLevelsModel) error {
	m.UsersDefault = types.Int64Value(int64(pl.UsersDefault))
	m.EventsDefault = types.Int64Value(int64(pl.EventsDefault))
	if pl.StateDefaultPtr != nil {
		m.StateDefault = types.Int64Value(int64(*pl.StateDefaultPtr))
	} else {
		m.StateDefault = types.Int64Null()
	}
	if pl.BanPtr != nil {
		m.Ban = types.Int64Value(int64(*pl.BanPtr))
	} else {
		m.Ban = types.Int64Null()
	}
	if pl.KickPtr != nil {
		m.Kick = types.Int64Value(int64(*pl.KickPtr))
	} else {
		m.Kick = types.Int64Null()
	}
	if pl.InvitePtr != nil {
		m.Invite = types.Int64Value(int64(*pl.InvitePtr))
	} else {
		m.Invite = types.Int64Null()
	}
	if pl.RedactPtr != nil {
		m.Redact = types.Int64Value(int64(*pl.RedactPtr))
	} else {
		m.Redact = types.Int64Null()
	}
	if len(pl.Users) == 0 {
		m.Users = types.MapNull(types.Int64Type)
	} else {
		raw := map[string]int64{}
		for k, v := range pl.Users {
			raw[string(k)] = int64(v)
		}
		val, d := types.MapValueFrom(ctx, types.Int64Type, raw)
		if d.HasError() {
			return errorFromDiags(d)
		}
		m.Users = val
	}
	if len(pl.Events) == 0 {
		m.Events = types.MapNull(types.Int64Type)
	} else {
		raw := map[string]int64{}
		for k, v := range pl.Events {
			raw[k] = int64(v)
		}
		val, d := types.MapValueFrom(ctx, types.Int64Type, raw)
		if d.HasError() {
			return errorFromDiags(d)
		}
		m.Events = val
	}
	if pl.Notifications != nil && pl.Notifications.RoomPtr != nil {
		m.NotifyRoom = types.Int64Value(int64(*pl.Notifications.RoomPtr))
	} else {
		m.NotifyRoom = types.Int64Null()
	}
	return nil
}

func (r *powerLevelsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan powerLevelsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	pl, err := powerLevelsFromModel(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid power_levels attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(plan.RoomID.ValueString()), event.StatePowerLevels, "", pl); err != nil {
		resp.Diagnostics.AddError("Failed to set m.room.power_levels", err.Error())
		return
	}
	plan.ID = plan.RoomID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *powerLevelsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state powerLevelsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var pl event.PowerLevelsEventContent
	found, err := getState(ctx, r.client, id.RoomID(state.RoomID.ValueString()), event.StatePowerLevels, "", &pl)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read m.room.power_levels", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if err := modelFromPowerLevels(ctx, &pl, &state); err != nil {
		resp.Diagnostics.AddError("Failed to map power_levels into state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *powerLevelsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior powerLevelsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	pl, err := powerLevelsFromModel(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid power_levels attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(plan.RoomID.ValueString()), event.StatePowerLevels, "", pl); err != nil {
		resp.Diagnostics.AddError("Failed to update m.room.power_levels", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *powerLevelsResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Power levels can't be deleted from a room; destroy drops only the state tracking.
}

func (r *powerLevelsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("room_id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
