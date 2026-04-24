package provider

import (
	"context"
	"os"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

type matrixProvider struct {
	version string
}

type matrixProviderModel struct {
	HomeserverURL  types.String `tfsdk:"homeserver_url"`
	AccessToken    types.String `tfsdk:"access_token"`
	UserID         types.String `tfsdk:"user_id"`
	RequestTimeout types.String `tfsdk:"request_timeout"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &matrixProvider{version: version}
	}
}

func (p *matrixProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "matrix"
	resp.Version = p.version
}

func (p *matrixProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage Matrix rooms, spaces, membership, and power levels from a user account.",
		Attributes: map[string]schema.Attribute{
			"homeserver_url": schema.StringAttribute{
				Description: "Base URL of the homeserver (e.g. https://matrix.org). Falls back to MATRIX_HOMESERVER_URL.",
				Optional:    true,
			},
			"access_token": schema.StringAttribute{
				Description: "Access token for the Matrix user. Falls back to MATRIX_ACCESS_TOKEN.",
				Optional:    true,
				Sensitive:   true,
			},
			"user_id": schema.StringAttribute{
				Description: "Full Matrix user ID (e.g. @alice:matrix.org). Falls back to MATRIX_USER_ID. If omitted, derived via /whoami.",
				Optional:    true,
			},
			"request_timeout": schema.StringAttribute{
				Description: "Per-request timeout as a Go duration string (e.g. \"30s\"). Default: 30s.",
				Optional:    true,
			},
		},
	}
}

func (p *matrixProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg matrixProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	homeserver := stringOr(cfg.HomeserverURL, os.Getenv("MATRIX_HOMESERVER_URL"))
	token := stringOr(cfg.AccessToken, os.Getenv("MATRIX_ACCESS_TOKEN"))
	userID := stringOr(cfg.UserID, os.Getenv("MATRIX_USER_ID"))
	timeoutStr := stringOr(cfg.RequestTimeout, "30s")

	if homeserver == "" {
		resp.Diagnostics.AddAttributeError(path.Root("homeserver_url"), "Missing homeserver URL",
			"Set provider attribute homeserver_url or env MATRIX_HOMESERVER_URL.")
	}
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("access_token"), "Missing access token",
			"Set provider attribute access_token or env MATRIX_ACCESS_TOKEN.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	timeout, err := time.ParseDuration(timeoutStr)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("request_timeout"), "Invalid duration", err.Error())
		return
	}

	mcli, err := mautrix.NewClient(homeserver, id.UserID(userID), token)
	if err != nil {
		resp.Diagnostics.AddError("Unable to build Matrix client", err.Error())
		return
	}
	mcli.Client.Timeout = timeout

	// Fail-fast creds check and auto-discover user_id when not set.
	who, err := mcli.Whoami(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Matrix authentication failed",
			"Could not call /whoami on "+homeserver+": "+err.Error())
		return
	}
	mcli.UserID = who.UserID
	mcli.DeviceID = who.DeviceID

	tflog.Info(ctx, "matrix provider configured", map[string]any{
		"homeserver": homeserver,
		"user_id":    string(who.UserID),
	})

	c := &Client{MX: mcli}
	resp.ResourceData = c
	resp.DataSourceData = c
}

func (p *matrixProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewRoomResource,
		NewSpaceResource,
		NewSpaceChildResource,
		NewRoomMemberResource,
		NewRoomPowerLevelsResource,
		NewRoomAliasResource,
		NewRoomStateResource,
		NewRoomJoinRulesResource,
		NewRoomServerACLResource,
	}
}

func (p *matrixProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewWhoamiDataSource,
		NewRoomDataSource,
		NewUserDataSource,
	}
}

func stringOr(v types.String, fallback string) string {
	if !v.IsNull() && !v.IsUnknown() && v.ValueString() != "" {
		return v.ValueString()
	}
	return fallback
}
