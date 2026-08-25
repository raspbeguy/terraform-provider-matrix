package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// TestRoomSchemaIncludesRoomOnlyAttrs guards that matrix_room exposes the
// room-only attributes (encryption_enabled, is_direct).
func TestRoomSchemaIncludesRoomOnlyAttrs(t *testing.T) {
	r := &roomResource{isSpace: false}
	resp := &fwresource.SchemaResponse{}
	r.Schema(context.Background(), fwresource.SchemaRequest{}, resp)

	for _, name := range []string{"encryption_enabled", "is_direct"} {
		if _, ok := resp.Schema.Attributes[name]; !ok {
			t.Errorf("matrix_room schema is missing %q", name)
		}
	}
}

// TestSpaceSchemaOmitsRoomOnlyAttrs guards that matrix_space does NOT expose
// encryption_enabled or is_direct, both of which are nonsensical on a space.
func TestSpaceSchemaOmitsRoomOnlyAttrs(t *testing.T) {
	r := &roomResource{isSpace: true}
	resp := &fwresource.SchemaResponse{}
	r.Schema(context.Background(), fwresource.SchemaRequest{}, resp)

	for _, name := range []string{"encryption_enabled", "is_direct"} {
		if _, ok := resp.Schema.Attributes[name]; ok {
			t.Errorf("matrix_space schema should not expose %q", name)
		}
	}
	// Sanity: the shared attributes are still present on the space variant.
	for _, name := range []string{"name", "topic", "history_visibility", "preset"} {
		if _, ok := resp.Schema.Attributes[name]; !ok {
			t.Errorf("matrix_space schema is missing shared attribute %q", name)
		}
	}
}

// createOnlyAttrs are the attributes /createRoom takes that the room does not
// report back the same way. Every one of them must be Optional+Computed with a
// plan modifier: Computed so a config that omits one does not diff against the
// value Read stores, and a modifier so the value survives the next plan.
//
// The shape is the whole fix for issue #32. Dropping Computed from any of them
// brings back the import that plans a replacement.
var createOnlyAttrs = []string{
	"preset", "visibility", "room_version", "room_alias_name", "initial_invites",
}

var roomOnlyCreateAttrs = []string{"encryption_enabled", "is_direct"}

func roomSchema(t *testing.T, isSpace bool) schema.Schema {
	t.Helper()
	r := &roomResource{isSpace: isSpace}
	resp := &fwresource.SchemaResponse{}
	r.Schema(context.Background(), fwresource.SchemaRequest{}, resp)
	return resp.Schema
}

func assertOptionalComputed(t *testing.T, s schema.Schema, name string) {
	t.Helper()
	attr, ok := s.Attributes[name]
	if !ok {
		t.Errorf("schema is missing %q", name)
		return
	}
	if !attr.IsOptional() || !attr.IsComputed() {
		t.Errorf("%q: optional=%v computed=%v, want both true", name, attr.IsOptional(), attr.IsComputed())
	}
	var mods int
	switch a := attr.(type) {
	case schema.StringAttribute:
		mods = len(a.PlanModifiers)
	case schema.BoolAttribute:
		mods = len(a.PlanModifiers)
	case schema.SetAttribute:
		mods = len(a.PlanModifiers)
	default:
		t.Errorf("%q: unexpected attribute type %T", name, attr)
		return
	}
	if mods == 0 {
		t.Errorf("%q: no plan modifiers, want at least UseStateForUnknown", name)
	}
}

// TestCreateOnlyAttrsAreOptionalComputed guards the schema shape on both the
// room and the space variant, which share one builder.
func TestCreateOnlyAttrsAreOptionalComputed(t *testing.T) {
	for _, isSpace := range []bool{false, true} {
		name := "room"
		if isSpace {
			name = "space"
		}
		t.Run(name, func(t *testing.T) {
			s := roomSchema(t, isSpace)
			for _, attr := range createOnlyAttrs {
				assertOptionalComputed(t, s, attr)
			}
			if isSpace {
				return
			}
			for _, attr := range roomOnlyCreateAttrs {
				assertOptionalComputed(t, s, attr)
			}
		})
	}
}

// TestReplaceIfWasKnownStr is the guard for issue #32 itself. RequiresReplace
// fires on null -> value as well as value -> value, and after an import every
// unread attribute is null, so a configured value would destroy the room the
// import was meant to adopt.
func TestReplaceIfWasKnownStr(t *testing.T) {
	cases := []struct {
		name  string
		state types.String
		plan  types.String
		want  bool
	}{
		{
			name:  "null prior state does not replace, this is the import case",
			state: types.StringNull(), plan: types.StringValue("public_chat"), want: false,
		},
		{
			name:  "a real change still replaces",
			state: types.StringValue("private_chat"), plan: types.StringValue("public_chat"), want: true,
		},
		{
			name:  "clearing a known value still replaces",
			state: types.StringValue("public_chat"), plan: types.StringNull(), want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := &planmodifier.StringResponse{PlanValue: c.plan}
			replaceIfWasKnownStr().PlanModifyString(context.Background(), planmodifier.StringRequest{
				// Non-null raw state and plan, so the framework's own create and
				// destroy guards do not short-circuit the check.
				State:      tfsdk.State{Raw: tftypes.NewValue(tftypes.String, "x")},
				Plan:       tfsdk.Plan{Raw: tftypes.NewValue(tftypes.String, "y")},
				StateValue: c.state,
				PlanValue:  c.plan,
			}, resp)
			if resp.RequiresReplace != c.want {
				t.Errorf("RequiresReplace = %v, want %v", resp.RequiresReplace, c.want)
			}
		})
	}
}

// TestUnset covers the rule the read helpers share: touch an attribute only when
// the model has no value of its own, which is null after an import and unknown
// while Create resolves a Computed attribute.
func TestUnset(t *testing.T) {
	cases := []struct {
		name string
		val  attr.Value
		want bool
	}{
		{"null", types.StringNull(), true},
		{"unknown", types.StringUnknown(), true},
		{"known", types.StringValue("11"), false},
		{"known empty string", types.StringValue(""), false},
		{"null bool", types.BoolNull(), true},
		{"known bool", types.BoolValue(false), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := unset(c.val); got != c.want {
				t.Errorf("unset(%v) = %v, want %v", c.val, got, c.want)
			}
		})
	}
}

// TestRoomAliasLocalpart checks the split that adopts room_alias_name from a
// room's canonical alias on import. id.RoomAlias has no Localpart method.
func TestRoomAliasLocalpart(t *testing.T) {
	cases := []struct {
		alias string
		want  string
	}{
		{"#general:example.com", "general"},
		{"#with-dashes:example.com", "with-dashes"},
		{"#room:sub.example.com:8448", "room"},
		{"", ""},
	}
	for _, c := range cases {
		t.Run(c.alias, func(t *testing.T) {
			_, got, _ := id.ParseCommonIdentifier(id.RoomAlias(c.alias))
			if got != c.want {
				t.Errorf("localpart of %q = %q, want %q", c.alias, got, c.want)
			}
		})
	}
}

// TestPresetAndVisibilityValidators guards the two enumerations the schema
// documents. A typo in preset used to destroy a room, because it forced a
// replacement before anything validated it.
func TestPresetAndVisibilityValidators(t *testing.T) {
	cases := []struct {
		attr    string
		value   string
		wantErr bool
	}{
		{"preset", "public_chat", false},
		{"preset", "private_chat", false},
		{"preset", "trusted_private_chat", false},
		{"preset", "public-chat", true},
		{"visibility", "public", false},
		{"visibility", "private", false},
		{"visibility", "Public", true},
	}
	s := roomSchema(t, false)
	for _, c := range cases {
		t.Run(c.attr+"="+c.value, func(t *testing.T) {
			sa, ok := s.Attributes[c.attr].(schema.StringAttribute)
			if !ok {
				t.Fatalf("%s is not a StringAttribute", c.attr)
			}
			if len(sa.Validators) == 0 {
				t.Fatalf("%s has no validators", c.attr)
			}
			resp := &validator.StringResponse{}
			sa.Validators[0].ValidateString(context.Background(), validator.StringRequest{
				Path:        path.Root(c.attr),
				ConfigValue: types.StringValue(c.value),
			}, resp)
			if resp.Diagnostics.HasError() != c.wantErr {
				t.Errorf("error = %v, want %v: %v", resp.Diagnostics.HasError(), c.wantErr, resp.Diagnostics)
			}
		})
	}
}

// TestWrongResourceTypeDetail guards the import type check. A space is a room
// with creation_content.type = m.space, so before this check either resource
// would adopt either kind and then fight it on every plan.
func TestWrongResourceTypeDetail(t *testing.T) {
	const roomID = "!abc:example.com"
	cases := []struct {
		name     string
		roomType event.RoomType
		isSpace  bool
		wantSub  string
	}{
		{name: "room into matrix_room", roomType: event.RoomTypeDefault, isSpace: false},
		{name: "space into matrix_space", roomType: event.RoomTypeSpace, isSpace: true},
		{
			name: "space into matrix_room", roomType: event.RoomTypeSpace, isSpace: false,
			wantSub: "is a matrix_space, not a matrix_room",
		},
		{
			name: "room into matrix_space", roomType: event.RoomTypeDefault, isSpace: true,
			wantSub: "is a matrix_room, not a matrix_space",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := wrongResourceTypeDetail(roomID, c.roomType, c.isSpace)
			if c.wantSub == "" {
				if got != "" {
					t.Errorf("want no error, got %q", got)
				}
				return
			}
			if !strings.Contains(got, c.wantSub) {
				t.Errorf("got %q, want it to contain %q", got, c.wantSub)
			}
			if !strings.Contains(got, roomID) {
				t.Errorf("message should name the room id; got %q", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Acceptance tests. These need TF_ACC and a live homeserver; resource.Test
// skips itself otherwise, so `go test ./...` on a PR is unaffected.
// ---------------------------------------------------------------------------

// testAccRoomImportConfig declares the create-only attributes that issue #32 is
// about. visibility is deliberately left out: both create and import take it
// from the directory, so the two sides agree whatever the homeserver's
// room_list_publication_rules do.
//
// room_version is pinned because the assertions must not move with the Synapse
// image, which is unpinned in the acceptance workflow.
func testAccRoomImportConfig(kind, alias string) string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    matrix = { source = "raspbeguy/matrix" }
  }
}

provider "matrix" {}

resource "matrix_%[1]s" "test" {
  name            = "tf-acc-import"
  topic           = "Managed by the acceptance suite"
  preset          = "private_chat"
  room_version    = "11"
  room_alias_name = %[2]q
}
`, kind, alias)
}

func testAccCaptureID(resourceName string, out *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", resourceName)
		}
		*out = rs.Primary.ID
		return nil
	}
}

func testAccCheckIDUnchanged(resourceName string, want *string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[resourceName]
		if !ok {
			return fmt.Errorf("resource %s not found in state", resourceName)
		}
		if rs.Primary.ID != *want {
			return fmt.Errorf("the room was replaced: id %s became %s (issue #32)", *want, rs.Primary.ID)
		}
		return nil
	}
}

// importDoesNotReplace is the body of both import tests. matrix_room and
// matrix_space share one schema builder, so both carried the defect.
//
// preset, is_direct and initial_invites cannot be read from any endpoint, so
// the import cannot reproduce them and the step after it settles them. That
// apply is the one that used to destroy the room.
func importDoesNotReplace(t *testing.T, kind, alias string) {
	t.Helper()
	testAccSkipUnlessAcc(t)
	name := "matrix_" + kind + ".test"
	config := testAccRoomImportConfig(kind, alias)
	var roomID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check:  testAccCaptureID(name, &roomID),
			},
			{
				ResourceName:            name,
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStatePersist:      true,
				ImportStateVerifyIgnore: []string{"preset", "is_direct", "initial_invites"},
			},
			{
				// Issue #32: this apply used to destroy the imported room and
				// build a new one, taking every dependent resource with it.
				Config: config,
				Check:  testAccCheckIDUnchanged(name, &roomID),
			},
			{
				// And now the config is fully settled.
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

func TestAccRoom_ImportDoesNotReplace(t *testing.T) {
	importDoesNotReplace(t, "room", "tf-acc-import-room")
}

func TestAccSpace_ImportDoesNotReplace(t *testing.T) {
	importDoesNotReplace(t, "space", "tf-acc-import-space")
}
