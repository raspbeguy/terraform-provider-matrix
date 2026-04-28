package provider

import (
	"context"

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
}

// leaveRoomBestEffort is used on resource Delete: we can't delete a room server-side,
// but we can leave it so it disappears from the caller's view.
func leaveRoomBestEffort(ctx context.Context, c *Client, roomID id.RoomID) error {
	_, err := c.MX.LeaveRoom(ctx, roomID)
	return err
}

func ptr[T any](v T) *T { return &v }
