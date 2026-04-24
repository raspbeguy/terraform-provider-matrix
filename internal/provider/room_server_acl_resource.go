package provider

import (
	"context"
	"fmt"
	"path"
	"strings"

	fwpath "github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

var (
	_ resource.Resource                = &serverACLResource{}
	_ resource.ResourceWithConfigure   = &serverACLResource{}
	_ resource.ResourceWithImportState = &serverACLResource{}
	_ resource.ResourceWithModifyPlan  = &serverACLResource{}
)

type serverACLResource struct{ client *Client }

type serverACLModel struct {
	ID              types.String `tfsdk:"id"`
	RoomID          types.String `tfsdk:"room_id"`
	Allow           types.Set    `tfsdk:"allow"`
	Deny            types.Set    `tfsdk:"deny"`
	AllowIPLiterals types.Bool   `tfsdk:"allow_ip_literals"`
}

func NewRoomServerACLResource() resource.Resource { return &serverACLResource{} }

func (r *serverACLResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_room_server_acl"
}

func (r *serverACLResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manages the m.room.server_acl state event for a room or space. Lets you block specific homeservers from federating events into the room.\n\n" +
			"**Warning — self-lockout risk.** A misconfigured ACL can permanently lock the caller's homeserver (and therefore the caller) out of the room: once the ACL blocks your server, you cannot send further events — including a corrective ACL. Recovery requires a homeserver admin to intervene. Before applying: make sure `allow` either contains your homeserver (or `\"*\"`) and `deny` does not match it. This provider emits a plan-time warning if it detects a likely self-lockout, but the final responsibility is yours.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				Description:   "Equal to room_id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"room_id": schema.StringAttribute{
				Required:      true,
				Description:   "ID of the room or space to manage the ACL for.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"allow": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Allow-list of homeserver globs (e.g. [\"*\"], or [\"matrix.org\", \"*.example.com\"]). If empty or unset, defaults to allowing all.",
			},
			"deny": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Deny-list of homeserver globs. Evaluated before allow.",
			},
			"allow_ip_literals": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Default:     booldefault.StaticBool(true),
				Description: "Whether server names that are IP literals are permitted. Default: true (per spec).",
			},
		},
	}
}

func (r *serverACLResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	c, err := clientFromResource(req)
	if err != nil {
		resp.Diagnostics.AddError("Provider configuration error", err.Error())
		return
	}
	r.client = c
}

func buildServerACLContent(ctx context.Context, m *serverACLModel) (*event.ServerACLEventContent, error) {
	c := &event.ServerACLEventContent{AllowIPLiterals: m.AllowIPLiterals.ValueBool()}
	if !m.Allow.IsNull() && !m.Allow.IsUnknown() {
		if d := m.Allow.ElementsAs(ctx, &c.Allow, false); d.HasError() {
			return nil, errorFromDiags(d)
		}
	}
	if !m.Deny.IsNull() && !m.Deny.IsUnknown() {
		if d := m.Deny.ElementsAs(ctx, &c.Deny, false); d.HasError() {
			return nil, errorFromDiags(d)
		}
	}
	return c, nil
}

// serverACLSelfLockoutWarnings returns human-readable warnings describing how the
// given ACL would lock the caller's homeserver out of the room, so callers can
// surface them as plan-time diagnostics. Pure function — no client/network access.
//
// Pattern matching uses path.Match (which supports * and ? globs and is what the
// Matrix spec's informal "shell glob" semantics boil down to in practice).
func serverACLSelfLockoutWarnings(homeserver string, c *event.ServerACLEventContent) []string {
	if homeserver == "" || c == nil {
		return nil
	}
	var out []string
	for _, pattern := range c.Deny {
		if globMatchHomeserver(pattern, homeserver) {
			out = append(out, fmt.Sprintf(
				"deny entry %q matches your own homeserver %q — applying this ACL will lock you out of the room, and only a homeserver admin can undo it.",
				pattern, homeserver))
		}
	}
	if len(c.Allow) > 0 {
		matched := false
		for _, pattern := range c.Allow {
			if globMatchHomeserver(pattern, homeserver) {
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, fmt.Sprintf(
				"allow list does not match your own homeserver %q — applying this ACL will lock you out of the room, and only a homeserver admin can undo it. Add %q (or %q) to allow.",
				homeserver, homeserver, "*"))
		}
	}
	return out
}

func globMatchHomeserver(pattern, server string) bool {
	ok, err := path.Match(pattern, server)
	return err == nil && ok
}

// serverACLInvalidPatternWarnings returns human-readable warnings for any
// entries in allow/deny whose glob syntax is malformed under path.Match
// semantics. Such patterns are silently treated as non-matching by the
// lockout detector (and may misbehave server-side too), so surfacing them
// at plan time catches typos early.
func serverACLInvalidPatternWarnings(c *event.ServerACLEventContent) []string {
	if c == nil {
		return nil
	}
	var out []string
	check := func(list []string, field string) {
		for _, p := range list {
			if _, err := path.Match(p, "probe.example"); err == path.ErrBadPattern {
				out = append(out, fmt.Sprintf(
					"%s entry %q is a malformed glob pattern (path.Match rejects it); it will be silently treated as non-matching by this provider's lockout checks and may behave unexpectedly server-side.",
					field, p))
			}
		}
	}
	check(c.Allow, "allow")
	check(c.Deny, "deny")
	return out
}

func callerHomeserver(c *Client) string {
	if c == nil {
		return ""
	}
	return homeserverFromMXID(string(c.MX.UserID))
}

// homeserverFromMXID extracts the server part of a Matrix ID. Returns "" for
// strings that don't look like mxids (missing colon, empty server part).
func homeserverFromMXID(mxid string) string {
	idx := strings.Index(mxid, ":")
	if idx < 0 || idx == len(mxid)-1 {
		return ""
	}
	return mxid[idx+1:]
}

// ModifyPlan surfaces the self-lockout warning at plan time so users see it
// before they run apply. Runs on both create and update plans; skipped on destroy.
func (r *serverACLResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // destroy plan — nothing to warn about
	}
	var plan serverACLModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	content, err := buildServerACLContent(ctx, &plan)
	if err != nil {
		return // malformed — Create/Update will surface the error
	}
	for _, w := range serverACLInvalidPatternWarnings(content) {
		resp.Diagnostics.AddWarning("Malformed ACL pattern", w)
	}
	for _, w := range serverACLSelfLockoutWarnings(callerHomeserver(r.client), content) {
		resp.Diagnostics.AddWarning("Potential server ACL self-lockout", w)
	}
}

func (r *serverACLResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan serverACLModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	content, err := buildServerACLContent(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid server_acl attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(plan.RoomID.ValueString()), event.StateServerACL, "", content); err != nil {
		resp.Diagnostics.AddError("Failed to set m.room.server_acl", err.Error())
		return
	}
	plan.ID = plan.RoomID
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serverACLResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state serverACLModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var c event.ServerACLEventContent
	found, err := getState(ctx, r.client, id.RoomID(state.RoomID.ValueString()), event.StateServerACL, "", &c)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read m.room.server_acl", err.Error())
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if len(c.Allow) == 0 {
		state.Allow = types.SetNull(types.StringType)
	} else {
		val, d := types.SetValueFrom(ctx, types.StringType, c.Allow)
		resp.Diagnostics.Append(d...)
		state.Allow = val
	}
	if len(c.Deny) == 0 {
		state.Deny = types.SetNull(types.StringType)
	} else {
		val, d := types.SetValueFrom(ctx, types.StringType, c.Deny)
		resp.Diagnostics.Append(d...)
		state.Deny = val
	}
	state.AllowIPLiterals = types.BoolValue(c.AllowIPLiterals)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *serverACLResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, prior serverACLModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = prior.ID
	content, err := buildServerACLContent(ctx, &plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid server_acl attributes", err.Error())
		return
	}
	if err := sendState(ctx, r.client, id.RoomID(plan.RoomID.ValueString()), event.StateServerACL, "", content); err != nil {
		resp.Diagnostics.AddError("Failed to update m.room.server_acl", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *serverACLResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// server ACL can't be truly deleted; destroy drops only the state tracking.
}

func (r *serverACLResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, fwpath.Root("room_id"), req.ID)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, fwpath.Root("id"), req.ID)...)
}
