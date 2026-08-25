package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// baseRoomModel holds the attributes shared by matrix_room and matrix_space.
type baseRoomModel struct {
	ID                types.String `tfsdk:"id"`
	Name              types.String `tfsdk:"name"`
	Topic             types.String `tfsdk:"topic"`
	AvatarURL         types.String `tfsdk:"avatar_url"`
	Preset            types.String `tfsdk:"preset"`
	Visibility        types.String `tfsdk:"visibility"`
	RoomVersion       types.String `tfsdk:"room_version"`
	RoomAliasName     types.String `tfsdk:"room_alias_name"`
	InitialInvites    types.Set    `tfsdk:"initial_invites"`
	CanonicalAlias    types.String `tfsdk:"canonical_alias"`
	HistoryVisibility types.String `tfsdk:"history_visibility"`
}

// roomModel is the tfsdk model for matrix_room. It adds room-only fields that
// are nonsensical on a space (encryption, direct-chat marker).
type roomModel struct {
	baseRoomModel
	Encryption types.Bool `tfsdk:"encryption_enabled"`
	IsDirect   types.Bool `tfsdk:"is_direct"`
}

// spaceModel is the tfsdk model for matrix_space. It deliberately omits
// encryption_enabled and is_direct so those attributes don't appear in the
// space's schema or its generated docs.
type spaceModel struct {
	baseRoomModel
}

// createRoomLike creates either a normal room or a space depending on isSpace.
// `encryption` and `isDirect` are room-only flags; they should be false when
// creating a space (the space variant doesn't expose those attributes).
// Returns the new room ID.
func createRoomLike(ctx context.Context, c *Client, m *baseRoomModel, encryption, isDirect, isSpace bool, diags *diag.Diagnostics) id.RoomID {
	req := &mautrix.ReqCreateRoom{
		Name:          m.Name.ValueString(),
		Topic:         m.Topic.ValueString(),
		Preset:        m.Preset.ValueString(),
		Visibility:    m.Visibility.ValueString(),
		RoomVersion:   id.RoomVersion(m.RoomVersion.ValueString()),
		RoomAliasName: m.RoomAliasName.ValueString(),
		IsDirect:      isDirect,
	}

	if isSpace {
		req.CreationContent = map[string]any{"type": "m.space"}
		// Element's defaults for spaces: messages locked to admins, invites at moderator level.
		// https://github.com/element-hq/element-web — applied here atomically with /createRoom
		// so there's no window where non-admins could post. Customize further via matrix_room_power_levels.
		invite := 50
		req.PowerLevelOverride = &event.PowerLevelsEventContent{
			EventsDefault: 100,
			InvitePtr:     &invite,
		}
	}

	if !m.InitialInvites.IsNull() && !m.InitialInvites.IsUnknown() {
		var invites []string
		diags.Append(m.InitialInvites.ElementsAs(ctx, &invites, false)...)
		if diags.HasError() {
			return ""
		}
		req.Invite = make([]id.UserID, len(invites))
		for i, u := range invites {
			req.Invite[i] = id.UserID(u)
		}
	}

	if !m.AvatarURL.IsNull() && m.AvatarURL.ValueString() != "" {
		uri, err := id.ParseContentURI(m.AvatarURL.ValueString())
		if err != nil {
			diags.AddAttributeError(path.Root("avatar_url"), "Invalid mxc URI", err.Error())
			return ""
		}
		req.InitialState = append(req.InitialState, &event.Event{
			Type:     event.StateRoomAvatar,
			StateKey: ptr(""),
			Content: event.Content{
				Parsed: &event.RoomAvatarEventContent{URL: uri.CUString()},
			},
		})
	}

	if encryption {
		req.InitialState = append(req.InitialState, &event.Event{
			Type:     event.StateEncryption,
			StateKey: ptr(""),
			Content: event.Content{
				Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1},
			},
		})
	}

	if s := m.HistoryVisibility.ValueString(); s != "" {
		req.InitialState = append(req.InitialState, &event.Event{
			Type:     event.StateHistoryVisibility,
			StateKey: ptr(""),
			Content: event.Content{
				Parsed: &event.HistoryVisibilityEventContent{HistoryVisibility: event.HistoryVisibility(s)},
			},
		})
	}

	resp, err := c.MX.CreateRoom(ctx, req)
	if err != nil {
		diags.AddError("Failed to create room", err.Error())
		return ""
	}
	// /createRoom carries visibility, but a homeserver is free to ignore it,
	// through its room_list_publication_rules. Read the directory back and say
	// so, rather than let the room quietly differ from the configuration. State
	// keeps the declared value: Terraform rejects an applied value that differs
	// from a known config value.
	if !m.Visibility.IsNull() && !m.Visibility.IsUnknown() {
		want := m.Visibility.ValueString()
		if got, found, err := getRoomVisibility(ctx, c, resp.RoomID); err == nil && found && got != want {
			diags.AddWarning("Directory visibility not honoured",
				"The homeserver reports "+string(resp.RoomID)+" as "+got+", not the requested "+want+
					". Synapse denies room directory publication by default, through its room_list_publication_rules, so this is the usual outcome rather than a misconfiguration."+
					" Every later plan will show this difference, because a refresh reads the directory."+
					visibilityCure)
		}
	}
	return resp.RoomID
}

// syncMutableStateFromModel sends the state events driven by the mutable attributes of
// a room-like resource. Called on Create (after CreateRoom) for fields not covered by
// ReqCreateRoom, and on Update whenever the plan differs from state.
func syncMutableStateFromModel(ctx context.Context, c *Client, roomID id.RoomID, plan, prior *baseRoomModel, diags *diag.Diagnostics) {
	// Name
	if !plan.Name.Equal(prior.Name) {
		if err := sendState(ctx, c, roomID, event.StateRoomName, "", &event.RoomNameEventContent{Name: plan.Name.ValueString()}); err != nil {
			diags.AddError("Failed to set room name", err.Error())
		}
	}
	// Topic
	if !plan.Topic.Equal(prior.Topic) {
		if err := sendState(ctx, c, roomID, event.StateTopic, "", &event.TopicEventContent{Topic: plan.Topic.ValueString()}); err != nil {
			diags.AddError("Failed to set room topic", err.Error())
		}
	}
	// Avatar
	if !plan.AvatarURL.Equal(prior.AvatarURL) {
		content := &event.RoomAvatarEventContent{}
		if s := plan.AvatarURL.ValueString(); s != "" {
			uri, err := id.ParseContentURI(s)
			if err != nil {
				diags.AddAttributeError(path.Root("avatar_url"), "Invalid mxc URI", err.Error())
				return
			}
			content.URL = uri.CUString()
		}
		if err := sendState(ctx, c, roomID, event.StateRoomAvatar, "", content); err != nil {
			diags.AddError("Failed to set room avatar", err.Error())
		}
	}
	// Directory visibility. Unlike the state events above this is not room state,
	// it is the homeserver's public room directory. A homeserver may refuse the
	// write, or accept it and not honour it, so read the value back and warn
	// rather than fail. State keeps the declared value either way: Terraform
	// rejects an applied value that differs from a known config value.
	if !plan.Visibility.IsNull() && !plan.Visibility.Equal(prior.Visibility) {
		want := plan.Visibility.ValueString()
		if err := setRoomVisibility(ctx, c, roomID, want); err != nil {
			diags.AddWarning("Failed to set directory visibility",
				"The homeserver refused to set the directory visibility of "+string(roomID)+" to "+want+
					": "+err.Error()+". Synapse denies room directory publication by default, through its room_list_publication_rules, so this is the usual outcome rather than a misconfiguration."+
					" Every later plan will show this difference, because a refresh reads the directory."+
					visibilityCure)
		} else if got, found, err := getRoomVisibility(ctx, c, roomID); err == nil && found && got != want {
			diags.AddWarning("Directory visibility not honoured",
				"The homeserver reports "+string(roomID)+" as "+got+" after it was set to "+want+
					". Synapse denies room directory publication by default, through its room_list_publication_rules, so this is the usual outcome rather than a misconfiguration."+
					" Every later plan will show this difference, because a refresh reads the directory."+
					visibilityCure)
		}
	}
	// History visibility. Skip when plan is null — the attribute is Optional+Computed,
	// so null means "accept whatever the server has." Only push a change when the
	// user explicitly declares a value that differs from state.
	if !plan.HistoryVisibility.IsNull() && !plan.HistoryVisibility.Equal(prior.HistoryVisibility) {
		content := &event.HistoryVisibilityEventContent{
			HistoryVisibility: event.HistoryVisibility(plan.HistoryVisibility.ValueString()),
		}
		if err := sendState(ctx, c, roomID, event.StateHistoryVisibility, "", content); err != nil {
			diags.AddError("Failed to set history_visibility", err.Error())
		}
	}
}

// readRoomLikeState populates the "live" attributes (name, topic, avatar, canonical alias)
// from the homeserver's state into m. Fields are zeroed to null when the state event is absent.
func readRoomLikeState(ctx context.Context, c *Client, roomID id.RoomID, m *baseRoomModel, diags *diag.Diagnostics) {
	// name
	var name event.RoomNameEventContent
	ok, err := getState(ctx, c, roomID, event.StateRoomName, "", &name)
	if err != nil {
		diags.AddError("Failed to read room name", err.Error())
		return
	}
	if ok && name.Name != "" {
		m.Name = types.StringValue(name.Name)
	} else {
		m.Name = types.StringNull()
	}

	// topic
	var topic event.TopicEventContent
	ok, err = getState(ctx, c, roomID, event.StateTopic, "", &topic)
	if err != nil {
		diags.AddError("Failed to read room topic", err.Error())
		return
	}
	if ok && topic.Topic != "" {
		m.Topic = types.StringValue(topic.Topic)
	} else {
		m.Topic = types.StringNull()
	}

	// avatar
	var avatar event.RoomAvatarEventContent
	ok, err = getState(ctx, c, roomID, event.StateRoomAvatar, "", &avatar)
	if err != nil {
		diags.AddError("Failed to read room avatar", err.Error())
		return
	}
	if ok && !avatar.URL.ParseOrIgnore().IsEmpty() {
		m.AvatarURL = types.StringValue(string(avatar.URL))
	} else {
		m.AvatarURL = types.StringNull()
	}

	// canonical alias
	var canon event.CanonicalAliasEventContent
	ok, err = getState(ctx, c, roomID, event.StateCanonicalAlias, "", &canon)
	if err != nil {
		diags.AddError("Failed to read canonical alias", err.Error())
		return
	}
	if ok && canon.Alias != "" {
		m.CanonicalAlias = types.StringValue(string(canon.Alias))
	} else {
		m.CanonicalAlias = types.StringNull()
	}

	// history visibility
	var hv event.HistoryVisibilityEventContent
	ok, err = getState(ctx, c, roomID, event.StateHistoryVisibility, "", &hv)
	if err != nil {
		diags.AddError("Failed to read history_visibility", err.Error())
		return
	}
	if ok && hv.HistoryVisibility != "" {
		m.HistoryVisibility = types.StringValue(string(hv.HistoryVisibility))
	} else {
		m.HistoryVisibility = types.StringNull()
	}

	readCreateOnlyState(ctx, c, roomID, m, &canon)
}

// readCreateOnlyState fills the attributes that /createRoom takes but that the
// room does not report back the same way. Without it, an imported room leaves
// them null and the first plan reads a configured value as a change. See #32.
//
// One rule for almost all of it: touch an attribute only when the model has no
// value of its own, meaning null after an import or unknown during Create.
// Anything the model already decided is left alone, because the provider cannot
// change these after creation and refreshing them would fight a value it can
// never reconcile.
//
// visibility is the exception, and is here only so that Create leaves it known.
// It is updatable, so a refresh must see the homeserver's value: that is
// refreshRoomVisibility, on the Read path. See issue #41.
//
// An unknown must always come out known, or Terraform rejects the apply. So
// every branch below ends by assigning something, null included, and a failed
// read costs a value rather than the whole refresh. Contrast the five reads in
// readRoomLikeState, where a missing event means a broken room and an error is
// the right answer.
func readCreateOnlyState(ctx context.Context, c *Client, roomID id.RoomID, m *baseRoomModel, canon *event.CanonicalAliasEventContent) {
	if unset(m.RoomVersion) {
		version := types.StringNull()
		if create, found, err := getCreateContent(ctx, c, roomID); err == nil && found {
			v := string(create.RoomVersion)
			if v == "" {
				// id.RoomV0 is the empty string, and the spec reads an absent
				// room_version as version 1.
				v = "1"
			}
			version = types.StringValue(v)
		}
		m.RoomVersion = version
	}

	if unset(m.RoomAliasName) {
		alias := types.StringNull()
		if localpart := adoptedAliasLocalpart(canon.Alias, callerHomeserver(c)); localpart != "" {
			alias = types.StringValue(localpart)
		}
		m.RoomAliasName = alias
	}

	if unset(m.Visibility) {
		visibility := types.StringNull()
		if vis, found, err := getRoomVisibility(ctx, c, roomID); err == nil && found && vis != "" {
			visibility = types.StringValue(vis)
		}
		m.Visibility = visibility
	}

	// preset is a creation-time macro over join rules, history visibility and
	// guest access, and no endpoint reports it. Deriving it from those three
	// would fight matrix_room_join_rules, which manages one of them directly.
	// initial_invites is a one-shot list with nothing to compare against.
	if m.Preset.IsUnknown() {
		m.Preset = types.StringNull()
	}
	if m.InitialInvites.IsUnknown() {
		m.InitialInvites = types.SetNull(types.StringType)
	}
}

// visibilityCure is appended to every directory warning. Without it a
// practitioner is told what went wrong and not what to do about the diff that
// now follows every plan.
const visibilityCure = " Remove `visibility` from the configuration to accept whatever the homeserver decides, and plans go clean again. Declare it only against a homeserver that is known to allow publication."

// refreshRoomVisibility re-reads the public room directory into m, whatever the
// model already holds.
//
// Read path only. readRoomLikeState runs on Create and Update too, and there the
// model carries the configured value; overwriting it with the homeserver's would
// make the applied value differ from a known config value, which Terraform
// rejects. On a refresh there is no configuration to contradict, so the
// homeserver wins. That is what makes a refused publication visible, instead of
// state claiming a value the homeserver never accepted. See issue #41.
//
// Best effort: a failed read leaves the prior value rather than breaking the
// refresh of every room in the workspace.
func refreshRoomVisibility(ctx context.Context, c *Client, roomID id.RoomID, m *baseRoomModel) {
	if vis, found, err := getRoomVisibility(ctx, c, roomID); err == nil && found && vis != "" {
		m.Visibility = types.StringValue(vis)
	}
}

// readRoomOnlyState does the same for the create-only attributes that live on
// roomModel rather than baseRoomModel, which readRoomLikeState cannot reach.
//
// is_direct has nothing to read: /createRoom only sets the flag on the invite
// member events, and the sender's own m.direct account data is a client
// convention that Synapse does not maintain.
func readRoomOnlyState(ctx context.Context, c *Client, roomID id.RoomID, m *roomModel) {
	if unset(m.Encryption) {
		encrypted := types.BoolNull()
		var enc event.EncryptionEventContent
		if found, err := getState(ctx, c, roomID, event.StateEncryption, "", &enc); err == nil {
			encrypted = types.BoolValue(found && enc.Algorithm != "")
		}
		m.Encryption = encrypted
	}
	if m.IsDirect.IsUnknown() {
		m.IsDirect = types.BoolNull()
	}
}

// adoptedAliasLocalpart returns the localpart of a room's canonical alias, for
// adopting into room_alias_name on import, or "" when it is not a value that
// attribute could have produced.
//
// room_alias_name is a localpart that /createRoom interprets on the caller's own
// homeserver, so an alias hosted anywhere else does not qualify. An unknown
// caller homeserver disqualifies everything, rather than matching by accident.
// id.RoomAlias has no Localpart method; this splitter strips the # sigil and
// splits off the server part.
func adoptedAliasLocalpart(alias id.RoomAlias, homeserver string) string {
	if alias == "" || homeserver == "" {
		return ""
	}
	_, localpart, server := id.ParseCommonIdentifier(alias)
	if server != homeserver {
		return ""
	}
	return localpart
}

// unset reports whether an attribute carries no value the model decided itself:
// null after an import, or unknown while Create resolves a Computed attribute.
func unset(v attr.Value) bool { return v.IsNull() || v.IsUnknown() }

// leaveRoom is used on resource Delete: we can't delete a room server-side,
// but we can leave it so it disappears from the caller's view.
func leaveRoom(ctx context.Context, c *Client, roomID id.RoomID) error {
	_, err := c.MX.LeaveRoom(ctx, roomID)
	return err
}

func ptr[T any](v T) *T { return &v }
