package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// memberWith is a member event that already carries a display name and an
// avatar, which is what a homeserver copies in from the global profile at join.
func memberWith() *event.MemberEventContent {
	return &event.MemberEventContent{
		Membership:  event.MembershipJoin,
		Displayname: "Global Name",
		AvatarURL:   id.ContentURIString("mxc://example.com/global"),
	}
}

// TestOverlayProfile_NullLeavesFieldAlone is the regression test for issue #39.
// A null attribute used to be written as an empty string, and both fields are
// omitempty, so that removed the key rather than restoring anything. Declaring
// only display_name stripped the user's per-room avatar.
func TestOverlayProfile_NullLeavesFieldAlone(t *testing.T) {
	current := memberWith()
	m := &userProfileOverrideModel{
		DisplayName: types.StringValue("PagerDuty"),
		AvatarURL:   types.StringNull(),
	}
	if err := overlayProfile(m, current); err != nil {
		t.Fatalf("overlayProfile: %v", err)
	}
	if current.Displayname != "PagerDuty" {
		t.Errorf("displayname: got %q, want the declared value", current.Displayname)
	}
	if current.AvatarURL != "mxc://example.com/global" {
		t.Errorf("avatar_url: got %q, want it untouched (issue #39)", current.AvatarURL)
	}
	if current.Membership != event.MembershipJoin {
		t.Errorf("membership must survive; got %q", current.Membership)
	}
}

// TestOverlayProfile_DeclaringNothingChangesNothing checks the whole event is
// left alone when the configuration declares neither field.
func TestOverlayProfile_DeclaringNothingChangesNothing(t *testing.T) {
	current := memberWith()
	m := &userProfileOverrideModel{DisplayName: types.StringNull(), AvatarURL: types.StringNull()}
	if err := overlayProfile(m, current); err != nil {
		t.Fatalf("overlayProfile: %v", err)
	}
	if current.Displayname != "Global Name" || current.AvatarURL != "mxc://example.com/global" {
		t.Errorf("nothing was declared, so nothing may change; got %+v", current)
	}
}

// TestOverlayProfile_EmptyStringClears covers the only way to remove an
// override, now that destroy leaves the member event alone.
func TestOverlayProfile_EmptyStringClears(t *testing.T) {
	t.Run("display_name", func(t *testing.T) {
		current := memberWith()
		m := &userProfileOverrideModel{DisplayName: types.StringValue(""), AvatarURL: types.StringNull()}
		if err := overlayProfile(m, current); err != nil {
			t.Fatalf("overlayProfile: %v", err)
		}
		if current.Displayname != "" {
			t.Errorf("an empty string must clear the field; got %q", current.Displayname)
		}
		if current.AvatarURL != "mxc://example.com/global" {
			t.Errorf("the other field must survive; got %q", current.AvatarURL)
		}
	})
	// mautrix accepts an empty string and returns an empty URI, so the clear path
	// needs no special case. Pin it: the path would break silently if that ever
	// tightened, and clearing is the only way to remove an override.
	t.Run("an empty avatar_url clears rather than erroring", func(t *testing.T) {
		current := memberWith()
		m := &userProfileOverrideModel{DisplayName: types.StringNull(), AvatarURL: types.StringValue("")}
		if err := overlayProfile(m, current); err != nil {
			t.Fatalf("an empty avatar_url must clear, not error: %v", err)
		}
		if current.AvatarURL != "" {
			t.Errorf("an empty string must clear the field; got %q", current.AvatarURL)
		}
	})
}

// TestOverlayProfile_InvalidAvatarErrors keeps the validation that was there.
func TestOverlayProfile_InvalidAvatarErrors(t *testing.T) {
	m := &userProfileOverrideModel{DisplayName: types.StringNull(), AvatarURL: types.StringValue("http://example.com/nope")}
	err := overlayProfile(m, memberWith())
	if err == nil {
		t.Fatal("want an error for a non-mxc URI")
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Error("the error must say something")
	}
}

// TestRefreshProfileField guards a perpetual-diff bug. A cleared field reads back
// from the server as absent, so recording null against a configuration holding
// an empty string would diff on every plan.
func TestRefreshProfileField(t *testing.T) {
	cases := []struct {
		name    string
		managed types.String
		server  string
		want    types.String
	}{
		{
			name:    "a server value wins",
			managed: types.StringValue("Old"), server: "New", want: types.StringValue("New"),
		},
		{
			name:    "a declared empty survives an absent server value",
			managed: types.StringValue(""), server: "", want: types.StringValue(""),
		},
		{
			name:    "a declared name against an absent value becomes null, so drift shows",
			managed: types.StringValue("Bot"), server: "", want: types.StringNull(),
		},
		{
			name:    "an unmanaged field against an absent value stays null",
			managed: types.StringNull(), server: "", want: types.StringNull(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := refreshProfileField(c.managed, c.server); !got.Equal(c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Acceptance. resource.Test skips itself unless TF_ACC is set.
// ---------------------------------------------------------------------------

func testAccOverrideConfig(displayName string) string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    matrix = { source = "raspbeguy/matrix" }
  }
}

provider "matrix" {}

data "matrix_whoami" "me" {}

resource "matrix_room" "test" {
  name         = "tf-acc-override"
  preset       = "private_chat"
  room_version = "11"
}

resource "matrix_user_profile_override" "test" {
  room_id      = matrix_room.test.id
  user_id      = data.matrix_whoami.me.user_id
  display_name = %[1]q
}
`, displayName)
}

// testAccSeedMemberAvatar puts an avatar on the caller's own m.room.member event
// through a raw client. Seeded rather than assumed: the CI account has no global
// avatar for the homeserver to copy in, so without this there is nothing for the
// apply to strip and the assertion could not fail.
func testAccSeedMemberAvatar(t *testing.T, roomResourceName, avatar string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[roomResourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", roomResourceName)
		}
		c := testAccClient(t)
		roomID, userID := id.RoomID(rs.Primary.ID), c.MX.UserID
		var member event.MemberEventContent
		found, err := getState(context.Background(), c, roomID, event.StateMember, string(userID), &member)
		if err != nil || !found {
			return fmt.Errorf("read m.room.member: found=%v err=%w", found, err)
		}
		member.AvatarURL = id.ContentURIString(avatar)
		if err := sendState(context.Background(), c, roomID, event.StateMember, string(userID), &member); err != nil {
			return fmt.Errorf("seed avatar: %w", err)
		}
		return nil
	}
}

// testAccCheckMemberProfile asserts on the member event the homeserver holds.
// Pass "" to require the field to be absent.
func testAccCheckMemberProfile(t *testing.T, roomResourceName, wantName, wantAvatar string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[roomResourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", roomResourceName)
		}
		c := testAccClient(t)
		var member event.MemberEventContent
		found, err := getState(context.Background(), c, id.RoomID(rs.Primary.ID), event.StateMember, string(c.MX.UserID), &member)
		if err != nil || !found {
			return fmt.Errorf("read m.room.member: found=%v err=%w", found, err)
		}
		if member.Displayname != wantName {
			return fmt.Errorf("displayname: got %q, want %q", member.Displayname, wantName)
		}
		if string(member.AvatarURL) != wantAvatar {
			return fmt.Errorf("avatar_url: got %q, want %q (issue #39)", member.AvatarURL, wantAvatar)
		}
		return nil
	}
}

// testAccCheckGlobalDisplayName asserts the room shows the account's global
// display name rather than an override, and that the avatar is untouched.
//
// The global name is read rather than hardcoded: it is whatever the CI
// homeserver gave the test account.
func testAccCheckGlobalDisplayName(t *testing.T, roomResourceName, wantAvatar string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		c := testAccClient(t)
		global, err := c.MX.GetDisplayName(context.Background(), c.MX.UserID)
		if err != nil {
			return fmt.Errorf("read global display name: %w", err)
		}
		return testAccCheckMemberProfile(t, roomResourceName, global.DisplayName, wantAvatar)(s)
	}
}

// TestAccUserProfileOverride_LeavesUndeclaredFieldAlone is the regression test
// for issue #39. A resource declaring only display_name used to write an empty
// avatar_url, and because the field is omitempty that removed the key, stripping
// the user's per-room avatar with nothing in the configuration asking for it.
func TestAccUserProfileOverride_LeavesUndeclaredFieldAlone(t *testing.T) {
	testAccSkipUnlessAcc(t)
	const avatar = "mxc://localhost/tfaccoverride"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Create the room and seed an avatar the apply could strip.
				Config: testAccOverrideConfig("Terrabot"),
				Check:  testAccSeedMemberAvatar(t, "matrix_room.test", avatar),
				// No ExpectNonEmptyPlan here, and that is the point. avatar_url is
				// not declared, so Read does not refresh it and the seed creates no
				// drift. On a Config step the flag *requires* a non-empty plan, so
				// setting it would fail. The assertions are in the next step.
			},
			{
				// The override declares display_name only, so the avatar stays.
				Config: testAccOverrideConfig("Terrabot renamed"),
				Check:  testAccCheckMemberProfile(t, "matrix_room.test", "Terrabot renamed", avatar),
			},
			{
				// An empty string removes the override. It does not leave the
				// field blank: a homeserver repopulates a member event that omits
				// displayname from the global profile, which Synapse does. So the
				// effect is "stop overriding", not "show nothing".
				Config: testAccOverrideConfig(""),
				Check:  testAccCheckGlobalDisplayName(t, "matrix_room.test", avatar),
			},
			{
				Config:   testAccOverrideConfig(""),
				PlanOnly: true,
			},
		},
	})
}
