package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var _ datasource.DataSource = &whoamiDataSource{}
var _ datasource.DataSourceWithConfigure = &whoamiDataSource{}

type whoamiDataSource struct {
	client *Client
}

type whoamiDataSourceModel struct {
	UserID   types.String `tfsdk:"user_id"`
	DeviceID types.String `tfsdk:"device_id"`
}

func NewWhoamiDataSource() datasource.DataSource {
	return &whoamiDataSource{}
}

func (d *whoamiDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_whoami"
}

func (d *whoamiDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Returns identity of the access token used by the provider.",
		Attributes: map[string]schema.Attribute{
			"user_id":   schema.StringAttribute{Computed: true},
			"device_id": schema.StringAttribute{Computed: true},
		},
	}
}

func (d *whoamiDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	c, err := clientFromDataSource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	d.client = c
}

func (d *whoamiDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.client == nil {
		return
	}
	who, err := d.client.MX.Whoami(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Matrix /whoami failed", err.Error())
		return
	}
	m := whoamiDataSourceModel{
		UserID:   types.StringValue(string(who.UserID)),
		DeviceID: types.StringValue(string(who.DeviceID)),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
