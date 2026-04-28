package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// TestRoomSchemaIncludesRoomOnlyAttrs guards that matrix_room exposes the
// room-only attributes (encryption_enabled, is_direct).
func TestRoomSchemaIncludesRoomOnlyAttrs(t *testing.T) {
	r := &roomResource{isSpace: false}
	resp := &resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, resp)

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
	resp := &resource.SchemaResponse{}
	r.Schema(context.Background(), resource.SchemaRequest{}, resp)

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
