package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
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
		Description: "Resolves a Matrix room by alias. State attributes are best-effort and require the caller to be a member.",
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

	// Best-effort state reads. Silently leave null if we aren't a member.
	cfg.Name = types.StringNull()
	cfg.Topic = types.StringNull()
	cfg.AvatarURL = types.StringNull()

	var name event.RoomNameEventContent
	if ok, _ := getState(ctx, d.client, res.RoomID, event.StateRoomName, "", &name); ok && name.Name != "" {
		cfg.Name = types.StringValue(name.Name)
	}
	var topic event.TopicEventContent
	if ok, _ := getState(ctx, d.client, res.RoomID, event.StateTopic, "", &topic); ok && topic.Topic != "" {
		cfg.Topic = types.StringValue(topic.Topic)
	}
	var avatar event.RoomAvatarEventContent
	if ok, _ := getState(ctx, d.client, res.RoomID, event.StateRoomAvatar, "", &avatar); ok && !avatar.URL.ParseOrIgnore().IsEmpty() {
		cfg.AvatarURL = types.StringValue(string(avatar.URL))
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
