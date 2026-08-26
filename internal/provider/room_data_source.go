package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var _ datasource.DataSource = &roomDataSource{}
var _ datasource.DataSourceWithConfigure = &roomDataSource{}

type roomDataSource struct{ client *Client }

type roomDataSourceModel struct {
	Alias     types.String `tfsdk:"alias"`
	RoomID    types.String `tfsdk:"room_id"`
	Servers   types.List   `tfsdk:"servers"`
	Name      types.String `tfsdk:"name"`
	Topic     types.String `tfsdk:"topic"`
	AvatarURL types.String `tfsdk:"avatar_url"`
}

func NewRoomDataSource() datasource.DataSource {
	return &roomDataSource{}
}

func (d *roomDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_room"
}

func (d *roomDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Resolves a Matrix room by alias.\n\n" +
			"`name`, `topic` and `avatar_url` are read from room state, which a homeserver shows to a member of the room, and to anyone when the room's history visibility is `world_readable`. They are null when the homeserver refuses to show them, and null when the room does not set them.\n\n" +
			"Any other failure to read them is an error. A data source has no earlier value to fall back on, so a null it reports flows into whatever consumes it, and a plan that removes a name because a gateway was restarting looks exactly like one that removes it on purpose.",
		Attributes: map[string]schema.Attribute{
			"alias":      schema.StringAttribute{Required: true},
			"room_id":    schema.StringAttribute{Computed: true},
			"servers":    schema.ListAttribute{ElementType: types.StringType, Computed: true},
			"name":       schema.StringAttribute{Computed: true},
			"topic":      schema.StringAttribute{Computed: true},
			"avatar_url": schema.StringAttribute{Computed: true},
		},
	}
}

func (d *roomDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	c, err := clientFromDataSource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	d.client = c
}

// readOptionalState reads a state event the caller may not be allowed to see.
//
// A refusal leaves the attribute null, which is the documented answer for a room
// the caller cannot read. Anything else is a failure to report: a data source
// has no earlier value to fall back on, so a null it invents flows onward and a
// plan can destroy a name because a gateway was restarting. See issue #60.
//
// getState already turns a 404 into found=false, so a room that simply does not
// set the event arrives here as an absent value rather than an error.
//
// It stands down once a read has failed, so one outage produces one diagnostic
// rather than three copies of it.
func (d *roomDataSource) readOptionalState(ctx context.Context, roomID id.RoomID,
	evtType event.Type, out any, diags *diag.Diagnostics) bool {
	if diags.HasError() {
		return false
	}
	found, err := getState(ctx, d.client, roomID, evtType, "", out)
	if err != nil {
		if forbiddenErr(err) {
			return false
		}
		diags.AddError("Failed to read "+evtType.Type,
			err.Error()+"\n\nThe attributes this data source reads from room state cannot be "+
				"trusted after this, so it reports the failure rather than a null that a plan "+
				"would read as a deliberate change.")
		return false
	}
	return found
}

func (d *roomDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg roomDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || d.client == nil {
		return
	}
	res, err := d.client.MX.ResolveAlias(ctx, id.RoomAlias(cfg.Alias.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Failed to resolve alias", err.Error())
		return
	}
	cfg.RoomID = types.StringValue(string(res.RoomID))
	servers, diag := types.ListValueFrom(ctx, types.StringType, res.Servers)
	resp.Diagnostics.Append(diag...)
	cfg.Servers = servers

	// A homeserver shows these to a member, and to anyone when the room is
	// world_readable. Null covers both "it will not show me" and "the room does
	// not set it". Anything else is a failure, and reporting it as null would
	// feed a wrong value to whatever consumes this. See issue #60.
	cfg.Name = types.StringNull()
	cfg.Topic = types.StringNull()
	cfg.AvatarURL = types.StringNull()

	var name event.RoomNameEventContent
	if d.readOptionalState(ctx, res.RoomID, event.StateRoomName, &name, &resp.Diagnostics) && name.Name != "" {
		cfg.Name = types.StringValue(name.Name)
	}
	var topic event.TopicEventContent
	if d.readOptionalState(ctx, res.RoomID, event.StateTopic, &topic, &resp.Diagnostics) && topic.Topic != "" {
		cfg.Topic = types.StringValue(topic.Topic)
	}
	var avatar event.RoomAvatarEventContent
	if d.readOptionalState(ctx, res.RoomID, event.StateRoomAvatar, &avatar, &resp.Diagnostics) && !avatar.URL.ParseOrIgnore().IsEmpty() {
		cfg.AvatarURL = types.StringValue(string(avatar.URL))
	}
	if resp.Diagnostics.HasError() {
		// Nothing is set from a read that failed, so no half-filled object
		// reaches a configuration that consumes it.
		return
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
