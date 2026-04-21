package provider

import (
	"context"
	"encoding/json"
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
	_ resource.Resource                = &roomStateResource{}
	_ resource.ResourceWithConfigure   = &roomStateResource{}
	_ resource.ResourceWithImportState = &roomStateResource{}
)

type roomStateResource struct{ client *Client }

type roomStateModel struct {
	ID          types.String `tfsdk:"id"`
	RoomID      types.String `tfsdk:"room_id"`
	EventType   types.String `tfsdk:"event_type"`
	StateKey    types.String `tfsdk:"state_key"`
	ContentJSON types.String `tfsdk:"content_json"`
}

func NewRoomStateResource() resource.Resource { return &roomStateResource{} }

func (r *roomStateResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_room_state"
}

func (r *roomStateResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	forceNew := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		Description: "Sends an arbitrary state event. Escape hatch for anything not covered by typed resources.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true, Description: "Composite: <room_id>|<event_type>|<state_key>.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"room_id":    schema.StringAttribute{Required: true, PlanModifiers: forceNew},
			"event_type": schema.StringAttribute{Required: true, PlanModifiers: forceNew},
			"state_key":  schema.StringAttribute{Optional: true, Computed: true, PlanModifiers: append(forceNew, stringplanmodifier.UseStateForUnknown()), Description: "Defaults to empty string."},
			"content_json": schema.StringAttribute{
				Required:      true,
				Description:   "JSON-encoded state event content. Compared semantically, so whitespace/key order don't trigger drift.",
				PlanModifiers: []planmodifier.String{jsonSemanticEqualityModifier{}},
			},
		},
	}
}

func (r *roomStateResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

func (r *roomStateResource) send(ctx context.Context, m *roomStateModel) error {
	var content any
	if err := json.Unmarshal([]byte(m.ContentJSON.ValueString()), &content); err != nil {
		return err
	}
	return sendState(ctx, r.client, id.RoomID(m.RoomID.ValueString()),
		event.NewEventType(m.EventType.ValueString()), m.StateKey.ValueString(), content)
}

func (r *roomStateResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan roomStateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.StateKey.IsNull() || plan.StateKey.IsUnknown() {
		plan.StateKey = types.StringValue("")
	}
	if err := r.send(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to set state event", err.Error())
		return
	}
	plan.ID = types.StringValue(plan.RoomID.ValueString() + "|" + plan.EventType.ValueString() + "|" + plan.StateKey.ValueString())
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomStateResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state roomStateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var raw map[string]any
	found, err := getState(ctx, r.client, id.RoomID(state.RoomID.ValueString()),
		event.NewEventType(state.EventType.ValueString()), state.StateKey.ValueString(), &raw)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read state event", err.Error())
		return
	}
	if !found || len(raw) == 0 {
		// Empty content means the caller (or another actor) cleared the event; drop it.
		resp.State.RemoveResource(ctx)
		return
	}
	buf, err := json.Marshal(raw)
	if err != nil {
		resp.Diagnostics.AddError("Failed to re-encode state event content", err.Error())
		return
	}
	state.ContentJSON = types.StringValue(string(buf))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *roomStateResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior roomStateModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	if err := r.send(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to update state event", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *roomStateResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state roomStateModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(state.RoomID.ValueString()),
		event.NewEventType(state.EventType.ValueString()), state.StateKey.ValueString(), map[string]any{}); err != nil {
		resp.Diagnostics.AddWarning("Failed to clear state event", err.Error())
	}
}

func (r *roomStateResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "|", 3)
	if len(parts) < 2 {
		resp.Diagnostics.AddError("Invalid import ID", "Expected <room_id>|<event_type>[|<state_key>]")
		return
	}
	stateKey := ""
	if len(parts) == 3 {
		stateKey = parts[2]
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("room_id"), parts[0])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("event_type"), parts[1])...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("state_key"), stateKey)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
