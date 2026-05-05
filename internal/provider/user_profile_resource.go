package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/id"
)

var (
	_ resource.Resource                = &userProfileResource{}
	_ resource.ResourceWithConfigure   = &userProfileResource{}
	_ resource.ResourceWithImportState = &userProfileResource{}
)

type userProfileResource struct{ client *Client }

type userProfileModel struct {
	ID          types.String `tfsdk:"id"`
	DisplayName types.String `tfsdk:"display_name"`
	AvatarURL   types.String `tfsdk:"avatar_url"`
}

func NewUserProfileResource() resource.Resource { return &userProfileResource{} }

func (r *userProfileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_user_profile"
}

func (r *userProfileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the caller's global profile (display name and avatar). Targets whichever user is authenticated by the provider's access_token — there's no way to set someone else's profile through a regular user token. Useful for bot accounts whose displayname/avatar should track config across deployments.\n\n" +
			"**Declare at most one `matrix_user_profile` per provider configuration.** Every instance resolves to the same identity (the caller's mxid), so multiple declarations race over the same global profile on every apply and the last one wins silently. If you need different profiles for different identities, configure additional providers with `alias` and a different access_token.\n\n" +
			"Destroy semantics: removing the resource drops it from state but leaves the profile as-is on the homeserver. Matrix has no protocol-level concept of a \"deleted\" profile, and clearing the displayname would render the bot as a raw mxid in clients — almost never what you want.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Matrix user ID of the caller, derived from the configured access_token.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"display_name": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Display name shown in clients. If unset, reflects whatever's currently on the server.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"avatar_url": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Avatar mxc:// URI. If unset, reflects whatever's currently on the server.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *userProfileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

// applyProfile pushes display_name and avatar_url to the homeserver. Null/unknown
// fields are skipped (null = "stop managing"; we don't clear server-side values).
// If prior is non-nil, fields equal to the prior value are also skipped — keeps
// Update from re-pushing unchanged values on every apply. Pass prior=nil on Create.
func (r *userProfileResource) applyProfile(ctx context.Context, plan, prior *userProfileModel, diags *diag.Diagnostics) {
	hadPrior := prior != nil
	if !hadPrior {
		prior = &userProfileModel{}
	}
	changed := func(planned, priorVal types.String) bool {
		if planned.IsNull() || planned.IsUnknown() {
			return false
		}
		return !hadPrior || !planned.Equal(priorVal)
	}
	if changed(plan.DisplayName, prior.DisplayName) {
		if err := r.client.MX.SetDisplayName(ctx, plan.DisplayName.ValueString()); err != nil {
			diags.AddError("Failed to set display_name", err.Error())
			return
		}
	}
	if changed(plan.AvatarURL, prior.AvatarURL) {
		uri, err := id.ParseContentURI(plan.AvatarURL.ValueString())
		if err != nil {
			diags.AddAttributeError(path.Root("avatar_url"), "Invalid mxc URI", err.Error())
			return
		}
		if err := r.client.MX.SetAvatarURL(ctx, uri); err != nil {
			diags.AddError("Failed to set avatar_url", err.Error())
			return
		}
	}
}

func (r *userProfileResource) refreshFromServer(ctx context.Context, m *userProfileModel) error {
	caller := r.client.MX.UserID
	m.ID = types.StringValue(string(caller))

	p, err := r.client.MX.GetProfile(ctx, caller)
	if err != nil {
		return err
	}
	if p.DisplayName != "" {
		m.DisplayName = types.StringValue(p.DisplayName)
	} else {
		m.DisplayName = types.StringNull()
	}
	if !p.AvatarURL.IsEmpty() {
		m.AvatarURL = types.StringValue(p.AvatarURL.String())
	} else {
		m.AvatarURL = types.StringNull()
	}
	return nil
}

func (r *userProfileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan userProfileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.applyProfile(ctx, &plan, nil, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.refreshFromServer(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to read back user profile", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userProfileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state userProfileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.refreshFromServer(ctx, &state); err != nil {
		resp.Diagnostics.AddError("Failed to read user profile", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *userProfileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior userProfileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	r.applyProfile(ctx, &plan, &prior, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.refreshFromServer(ctx, &plan); err != nil {
		resp.Diagnostics.AddError("Failed to read back user profile", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *userProfileResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Profile fields can't be meaningfully "deleted" — dropping them would render
	// the user as a raw mxid in clients. Destroy drops only the state tracking.
}

func (r *userProfileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Validate that the import ID matches the caller's mxid. Since this resource
	// always targets the caller, any other ID is misleading.
	caller := string(r.client.MX.UserID)
	if req.ID != caller {
		resp.Diagnostics.AddError("Import ID must match the caller's mxid",
			"matrix_user_profile only manages the caller's profile ("+caller+"); the import ID was "+req.ID+".")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), caller)...)
}
