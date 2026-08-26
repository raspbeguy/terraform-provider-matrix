package provider

import (
	"context"
	"fmt"
	"testing"

	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"maunium.net/go/mautrix/event"
)

func spaceChildSchema(t *testing.T) schema.Schema {
	t.Helper()
	r := &spaceChildResource{}
	resp := &fwresource.SchemaResponse{}
	r.Schema(context.Background(), fwresource.SchemaRequest{}, resp)
	return resp.Schema
}

func mustSet(t *testing.T, values ...string) types.Set {
	t.Helper()
	v, d := types.SetValueFrom(context.Background(), types.StringType, values)
	if d.HasError() {
		t.Fatalf("SetValueFrom: %v", d)
	}
	return v
}

// TestSpaceChildContentAttrsAreOptionalComputed guards the shape the whole fix
// rests on. Without Computed, an m.space.child with no suggested key reads back
// as false and a configuration omitting it plans false -> null forever, so an
// import never finishes. See issue #40.
func TestSpaceChildContentAttrsAreOptionalComputed(t *testing.T) {
	s := spaceChildSchema(t)
	for _, name := range []string{"via", "order", "suggested"} {
		attr, ok := s.Attributes[name]
		if !ok {
			t.Errorf("schema is missing %q", name)
			continue
		}
		if !attr.IsOptional() || !attr.IsComputed() {
			t.Errorf("%q: optional=%v computed=%v, want both true", name, attr.IsOptional(), attr.IsComputed())
		}
		var mods int
		switch a := attr.(type) {
		case schema.SetAttribute:
			mods = len(a.PlanModifiers)
		case schema.StringAttribute:
			mods = len(a.PlanModifiers)
		case schema.BoolAttribute:
			mods = len(a.PlanModifiers)
		default:
			t.Errorf("%q: unexpected type %T", name, attr)
			continue
		}
		if mods == 0 {
			t.Errorf("%q: no plan modifiers, want UseStateForUnknown", name)
		}
	}
}

// TestResolveUnknownSpaceChild guards the issue #29 failure mode. Create writes
// the plan straight to state, so every Computed attribute has to come out known
// or the apply fails with "Provider returned invalid result object after apply".
func TestResolveUnknownSpaceChild(t *testing.T) {
	content := &event.SpaceChildEventContent{Via: []string{"example.com"}, Order: "01", Suggested: true}

	t.Run("every unknown is filled", func(t *testing.T) {
		m := &spaceChildModel{
			Via:       types.SetUnknown(types.StringType),
			Order:     types.StringUnknown(),
			Suggested: types.BoolUnknown(),
		}
		if err := resolveUnknownSpaceChild(context.Background(), content, m); err != nil {
			t.Fatalf("resolveUnknownSpaceChild: %v", err)
		}
		for name, v := range map[string]interface{ IsUnknown() bool }{
			"via": m.Via, "order": m.Order, "suggested": m.Suggested,
		} {
			if v.IsUnknown() {
				t.Errorf("%s is still unknown after apply (issue #29)", name)
			}
		}
		if m.Order.ValueString() != "01" || !m.Suggested.ValueBool() {
			t.Errorf("resolved wrong values: %+v", m)
		}
	})

	t.Run("known plan values survive", func(t *testing.T) {
		m := &spaceChildModel{
			Via:       mustSet(t, "declared.example"),
			Order:     types.StringNull(),
			Suggested: types.BoolValue(false),
		}
		if err := resolveUnknownSpaceChild(context.Background(), content, m); err != nil {
			t.Fatalf("resolveUnknownSpaceChild: %v", err)
		}
		if !m.Order.IsNull() {
			t.Errorf("a planned null was overwritten: %v", m.Order)
		}
		if m.Suggested.ValueBool() {
			t.Errorf("a planned false was overwritten: %v", m.Suggested)
		}
		var via []string
		if d := m.Via.ElementsAs(context.Background(), &via, false); d.HasError() {
			t.Fatalf("ElementsAs: %v", d)
		}
		if len(via) != 1 || via[0] != "declared.example" {
			t.Errorf("a planned via was overwritten: %v", via)
		}
	})
}

// TestModelFromSpaceChild covers what an absent key maps to. suggested must
// always come out known, which is the attribute issue #40 is about.
func TestModelFromSpaceChild(t *testing.T) {
	var m spaceChildModel
	if err := modelFromSpaceChild(context.Background(), &event.SpaceChildEventContent{}, &m); err != nil {
		t.Fatalf("modelFromSpaceChild: %v", err)
	}
	if !m.Via.IsNull() || !m.Order.IsNull() {
		t.Errorf("absent via and order must map to null; got %v %v", m.Via, m.Order)
	}
	if m.Suggested.IsNull() || m.Suggested.IsUnknown() || m.Suggested.ValueBool() {
		t.Errorf("absent suggested must map to a known false; got %v", m.Suggested)
	}
}

// TestSpaceChildMissingVia covers the plan-time guard. The specification requires
// via, and a link with no via, order or suggested is indistinguishable from a
// removed one, because removal is done by writing empty content.
func TestSpaceChildMissingVia(t *testing.T) {
	cases := []struct {
		name      string
		planVia   types.Set
		configVia types.Set
		want      bool
	}{
		{name: "declared and non-empty", planVia: mustSet(t, "example.com"), configVia: mustSet(t, "example.com")},
		{
			name: "declared but empty", planVia: mustSet(t), configVia: mustSet(t), want: true,
		},
		{
			name: "planned null", planVia: types.SetNull(types.StringType), configVia: types.SetNull(types.StringType), want: true,
		},
		{
			// A create declaring nothing. The plan cannot tell "not declared"
			// from "will be computed", so the configuration decides.
			name: "unknown plan, nothing in the config", planVia: types.SetUnknown(types.StringType),
			configVia: types.SetNull(types.StringType), want: true,
		},
		{
			// An update that omits via inherits the space's own value.
			name: "unknown plan, but the config declares it", planVia: types.SetUnknown(types.StringType),
			configVia: mustSet(t, "example.com"),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := spaceChildMissingVia(c.planVia, c.configVia); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Acceptance
// ---------------------------------------------------------------------------

func testAccSpaceChildConfig() string {
	return fmt.Sprintf(`
terraform {
  required_providers {
    matrix = { source = "raspbeguy/matrix" }
  }
}

provider "matrix" {}

resource "matrix_space" "parent" {
  name         = "tf-acc-space-child-parent"
  preset       = "private_chat"
  room_version = "11"
}

resource "matrix_room" "child" {
  name         = "tf-acc-space-child-child"
  preset       = "private_chat"
  room_version = "11"
}

resource "matrix_space_child" "test" {
  parent_space_id = matrix_space.parent.id
  child_room_id   = matrix_room.child.id
  via             = [%[1]q]
}
`, "localhost")
}

// TestAccSpaceChild_ImportPlansClean is the regression test for issue #40. The
// configuration declares only via, so suggested and order are Computed. Before
// the fix Read stored suggested as false while the configuration held null, and
// every plan after an import wanted a write to the parent space.
func TestAccSpaceChild_ImportPlansClean(t *testing.T) {
	testAccSkipUnlessAcc(t)
	const name = "matrix_space_child.test"
	config := testAccSpaceChildConfig()

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					// Resolved rather than left unknown, which is the issue #29 trap.
					resource.TestCheckResourceAttr(name, "suggested", "false"),
					resource.TestCheckResourceAttr(name, "via.#", "1"),
				),
			},
			{Config: config, PlanOnly: true},
			{
				ResourceName:      name,
				ImportState:       true,
				ImportStateVerify: true,
			},
			// The regression test: this planned a write before the fix.
			{Config: config, PlanOnly: true},
		},
	})
}
