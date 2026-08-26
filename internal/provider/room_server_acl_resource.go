package provider

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
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
			"**Warning: a bad ACL cannot be undone.** A homeserver applies this event to the events that arrive from other servers. If the ACL blocks your own server, every remote server rejects this room's events from you, and rejects a corrective ACL too. Your own server still accepts your events, so local users see no change while federation stays broken for good.\n\n" +
			"Before you apply, make sure that `allow` contains your homeserver or `\"*\"`, and that `deny` does not match it. This provider warns at plan time when it detects a likely self-lockout, and refuses a plan that denies every server.",
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
				Description: "Allow-list of homeserver globs, for example [\"*\"] or [\"matrix.org\", \"*.example.com\"].\n\n" +
					"An empty or unset list denies every server, your own included, so use [\"*\"] to allow all. This provider refuses such a plan.\n\n" +
					"Matching uses the glob the Matrix specification defines: `*` matches zero or more characters, `?` matches exactly one, every other character is literal, and case is ignored.",
			},
			"deny": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Deny-list of homeserver globs. A homeserver checks this list before allow, so a name that both lists match is denied.",
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

// aclLockoutConsequence says what an ACL that blocks your own server costs. A
// homeserver applies this event only to what arrives from other servers, so your
// own server keeps accepting your events. See federation_server.py, where every
// check runs against the origin of an inbound request.
const aclLockoutConsequence = " Applying this ACL blocks your own server: every remote server " +
	"rejects this room's events from you after that, and rejects a corrective ACL too, so the " +
	"room never federates again."

// serverACLSelfLockoutWarnings returns readable warnings that describe how the
// given ACL blocks the caller's own homeserver, so a caller can surface them as
// plan-time diagnostics. Pure function, with no client or network access.
//
// Matching follows globMatchHomeserver, which implements the specification's
// glob rather than path.Match. See issue #61.
func serverACLSelfLockoutWarnings(homeserver string, c *event.ServerACLEventContent) []string {
	if homeserver == "" || c == nil {
		return nil
	}
	var out []string
	// Synapse rejects a bracketed name, or one that parses as IPv4, before it
	// looks at either list. So this flag is a lockout route of its own.
	if !c.AllowIPLiterals && isIPLiteral(homeserver) {
		out = append(out, fmt.Sprintf(
			"allow_ip_literals is false and your own homeserver %q is an IP literal."+aclLockoutConsequence,
			homeserver))
	}
	for _, pattern := range c.Deny {
		if globMatchHomeserver(pattern, homeserver) {
			out = append(out, fmt.Sprintf(
				"deny entry %q matches your own homeserver %q."+aclLockoutConsequence,
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
				"allow list does not match your own homeserver %q."+aclLockoutConsequence+
					" Add %q or %q to allow.",
				homeserver, homeserver, "*"))
		}
	}
	return out
}

// globMatchHomeserver reports whether pattern matches server under the glob the
// Matrix specification defines for m.room.server_acl. "*" matches zero or more
// characters, "?" matches exactly one, and every other character is literal.
//
// This used to call path.Match, which is a different language. Synapse builds
// its matcher with glob_to_regex in rust/src/push/utils.rs, which regex-escapes
// every run between wildcards and compiles the result case-insensitively. So
// there is no character class, there is no malformed pattern, and the match
// ignores case. path.Match agreed with none of that, which hid real lockouts
// and invented false ones. See issue #61.
//
// A server name is ASCII by grammar, so a comparison of bytes after lowering is
// safe here.
func globMatchHomeserver(pattern, server string) bool {
	p := strings.ToLower(pattern)
	s := strings.ToLower(server)
	// Two pointers, with a backtrack point at the most recent "*".
	pi, si, star, retry := 0, 0, -1, 0
	for si < len(s) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == s[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star, retry = pi, si
			pi++
		case star >= 0:
			// The last "*" took too little. Give it one more character.
			retry++
			pi, si = star+1, retry
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// isIPLiteral reports whether a server name is an IP literal, the way Synapse
// decides it in rust/src/acl/mod.rs: an IPv6 literal is bracketed, and an IPv4
// literal must parse whole. A name that carries a port does not parse, and
// Synapse lets that through, so this does not strip one either. A colon rules
// out the IPv4-mapped form, which Rust rejects and Go accepts.
func isIPLiteral(server string) bool {
	if strings.HasPrefix(server, "[") {
		return true
	}
	if strings.Contains(server, ":") {
		return false
	}
	ip := net.ParseIP(server)
	return ip != nil && ip.To4() != nil
}

// serverACLDenyAllDiag refuses an ACL whose allow list is empty.
//
// A homeserver checks deny first, then allow, and rejects everything that
// matched neither. So an empty allow list denies every server, which is the
// opposite of what this resource used to document. See rust/src/acl/mod.rs and
// issue #57.
//
// This is an error rather than a warning because the damage cannot be undone
// from this side. A deny-all ACL has no use that automation needs.
// It reads the model rather than the built content, because an unknown allow
// list and an empty one both reach event.ServerACLEventContent as a nil slice.
// An allow list computed from another resource is unknown at plan time, and an
// error on that would refuse a valid configuration.
func serverACLDenyAllDiag(homeserver string, m *serverACLModel) diag.Diagnostics {
	var diags diag.Diagnostics
	if m == nil || m.Allow.IsUnknown() {
		return diags
	}
	if !m.Allow.IsNull() && len(m.Allow.Elements()) > 0 {
		return diags
	}
	own := "your own homeserver"
	if homeserver != "" {
		own = "your own homeserver " + homeserver
	}
	diags.AddAttributeError(fwpath.Root("allow"), "The allow list denies every server",
		"allow is empty, so this ACL denies every homeserver, "+own+" included. Every remote "+
			"server rejects this room's events from your server after that, and rejects a "+
			"corrective ACL too, so the room never federates again. Your own server still "+
			"accepts your events, so local users see no change.\n\n"+
			"Set allow = [\"*\"] to allow every server, then name what you want to block in deny.")
	return diags
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

// ModifyPlan judges the ACL before an apply sends it. A plan that denies every
// server is refused, and a likely self-lockout is a warning. It runs on create
// and update plans, and is skipped on destroy, where there is nothing to send.
func (r *serverACLResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // a destroy plan has nothing to warn about
	}
	var plan serverACLModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	content, err := buildServerACLContent(ctx, &plan)
	if err != nil {
		return // malformed: Create and Update surface the error
	}
	// The provider may not be configured yet, in which case this is empty and
	// the diagnostics below word themselves without a server name.
	homeserver := callerHomeserver(r.client)
	// A deny-all ACL is refused rather than warned about. See issue #57.
	resp.Diagnostics.Append(serverACLDenyAllDiag(homeserver, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, w := range serverACLSelfLockoutWarnings(homeserver, content) {
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
