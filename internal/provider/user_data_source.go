package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/id"
)

var _ datasource.DataSource = &userDataSource{}
var _ datasource.DataSourceWithConfigure = &userDataSource{}

type userDataSource struct{ client *Client }

type userDataSourceModel struct {
	UserID      types.String `tfsdk:"user_id"`
	DisplayName types.String `tfsdk:"display_name"`
	AvatarURL   types.String `tfsdk:"avatar_url"`
}

func NewUserDataSource() datasource.DataSource { return &userDataSource{} }

func (d *userDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user"
}

func (d *userDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Reads a user's public profile.",
		Attributes: map[string]schema.Attribute{
			"user_id":      schema.StringAttribute{Required: true},
			"display_name": schema.StringAttribute{Computed: true},
			"avatar_url":   schema.StringAttribute{Computed: true},
		},
	}
}

func (d *userDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	c, err := clientFromDataSource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	d.client = c
}

func (d *userDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg userDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() || d.client == nil {
		return
	}
	p, err := d.client.MX.GetProfile(ctx, id.UserID(cfg.UserID.ValueString()))
	if err != nil {
		resp.Diagnostics.AddError("Failed to fetch profile", err.Error())
		return
	}
	if p.DisplayName != "" {
		cfg.DisplayName = types.StringValue(p.DisplayName)
	} else {
		cfg.DisplayName = types.StringNull()
	}
	if !p.AvatarURL.IsEmpty() {
		cfg.AvatarURL = types.StringValue(p.AvatarURL.String())
	} else {
		cfg.AvatarURL = types.StringNull()
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &cfg)...)
}
