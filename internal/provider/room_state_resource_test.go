package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"maunium.net/go/mautrix/event"
)

// TestRoomStateOwnershipDiag covers the rule nothing enforced before issue #58.
// matrix_room_state calls itself the escape hatch for anything not covered by a
// typed resource, so an event type that is covered must be refused.
func TestRoomStateOwnershipDiag(t *testing.T) {
	// A loop over an empty table passes without testing anything.
	if len(stateEventOwners) < 10 {
		t.Fatalf("stateEventOwners has %d rows; the provider models more than that", len(stateEventOwners))
	}
	for eventType, owner := range stateEventOwners {
		t.Run(eventType, func(t *testing.T) {
			got := roomStateOwnershipDiag(types.StringValue(eventType))
			if !got.HasError() {
				t.Fatalf("%s is owned by %s and must be refused", eventType, owner)
			}
			detail := got[0].Detail()
			if !strings.Contains(detail, owner) {
				t.Errorf("the error must name %s so the reader knows what to use; got %q", owner, detail)
			}
			// The two whose clearing cannot be undone say so, because that is
			// why this is an error rather than a warning.
			_, locks := clearingLocksRoom(eventType)
			if locks && !strings.Contains(detail, "Destroying this resource") {
				t.Errorf("%s must also explain what a destroy would do; got %q", eventType, detail)
			}
		})
	}
}

func TestRoomStateOwnershipDiag_Allowed(t *testing.T) {
	cases := []struct {
		name      string
		eventType types.String
	}{
		{name: "a custom event type", eventType: types.StringValue("com.example.custom")},
		{name: "an event no typed resource covers", eventType: types.StringValue("m.room.pinned_events")},
		// A type computed from another resource. Refusing this would break a
		// valid configuration.
		{name: "an unknown type is not judged yet", eventType: types.StringUnknown()},
		{name: "a null type", eventType: types.StringNull()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := roomStateOwnershipDiag(tc.eventType); got.HasError() {
				t.Errorf("must be allowed, got %v", got)
			}
		})
	}
}

// TestClearingLocksRoom pins which events must never have their content
// cleared. Matrix has no way to remove a state event, so a destroy publishes
// empty content, and for these two that cannot be undone.
func TestClearingLocksRoom(t *testing.T) {
	for _, eventType := range []string{event.StatePowerLevels.Type, event.StateServerACL.Type} {
		why, locks := clearingLocksRoom(eventType)
		if !locks {
			t.Errorf("%s must refuse to be cleared", eventType)
			continue
		}
		if why == "" {
			t.Errorf("%s gives no reason", eventType)
		}
	}
	// Clearing content is the documented way to remove a space child, so that
	// one must stay allowed.
	for _, eventType := range []string{
		event.StateSpaceChild.Type,
		event.StateRoomName.Type,
		"m.room.pinned_events",
		"com.example.custom",
	} {
		if _, locks := clearingLocksRoom(eventType); locks {
			t.Errorf("%s is safe to clear and must not be refused", eventType)
		}
	}
}

// TestStateEventOwnersIsComplete keeps the table honest.
//
// A hand-written table goes stale the moment a typed resource is added, and the
// guard then silently stops covering the new event while every other test still
// passes. This reads the package's own source instead: every event.State*
// constant the provider sends or reads must appear in the table.
//
// Reads count as well as writes. Three of the twelve entries never reach
// sendState at all, because /createRoom carries them, so a test that watched
// only writes would cover nine rows while claiming to cover all of them.
func TestStateEventOwnersIsComplete(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no source files found: %v", err)
	}

	sent := map[string][]string{} // constant name -> files that use it
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "room_state_resource.go" {
			continue
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name, ok := call.Fun.(*ast.Ident)
			if !ok || (name.Name != "sendState" && name.Name != "getState") {
				return true
			}
			for _, arg := range call.Args {
				sel, ok := arg.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "event" || !strings.HasPrefix(sel.Sel.Name, "State") {
					continue
				}
				sent[sel.Sel.Name] = append(sent[sel.Sel.Name], file)
			}
			return true
		})
	}

	// A guard that inspects nothing passes for the wrong reason. Every entry in
	// the table is reachable this way, so anything less means the scan broke.
	if len(sent) < len(stateEventOwners) {
		t.Fatalf("found %d state events in the source but the table has %d rows; the scan is "+
			"missing call sites it used to see", len(sent), len(stateEventOwners))
	}

	byConstant := map[string]string{
		"StatePowerLevels":       event.StatePowerLevels.Type,
		"StateServerACL":         event.StateServerACL.Type,
		"StateJoinRules":         event.StateJoinRules.Type,
		"StateMember":            event.StateMember.Type,
		"StateSpaceChild":        event.StateSpaceChild.Type,
		"StateRoomName":          event.StateRoomName.Type,
		"StateTopic":             event.StateTopic.Type,
		"StateRoomAvatar":        event.StateRoomAvatar.Type,
		"StateHistoryVisibility": event.StateHistoryVisibility.Type,
		"StateCanonicalAlias":    event.StateCanonicalAlias.Type,
		"StateEncryption":        event.StateEncryption.Type,
		"StateCreate":            event.StateCreate.Type,
	}
	for constant, files := range sent {
		eventType, known := byConstant[constant]
		if !known {
			t.Errorf("%s is used by %v and this test does not know it. Add it here and to "+
				"stateEventOwners, or matrix_room_state can fight over it (issue #58).",
				constant, files)
			continue
		}
		if _, owned := stateEventOwners[eventType]; !owned {
			t.Errorf("%s (%s) is used by %v but is missing from stateEventOwners, so "+
				"matrix_room_state can own it too (issue #58).", constant, eventType, files)
		}
	}
}

// TestRoomStateGuardsAreWired reads the resource's own source.
//
// Every guard here is a pure function, so it keeps passing after the method
// that used to call it stops calling it. Three wirings matter:
//
//   - ModifyPlan returns early on a destroy plan. Without it, a resource that
//     predates the ownership check, or one that was imported, cannot be
//     destroyed at all while its configuration block is present.
//   - ModifyPlan refuses an owned event type.
//   - Delete refuses to clear the two events whose clearing locks the room.
//
// See issue #58.
func TestRoomStateGuardsAreWired(t *testing.T) {
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "room_state_resource.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	bodies := map[string]*ast.BlockStmt{}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv != nil && fn.Body != nil {
			bodies[fn.Name.Name] = fn.Body
		}
	}

	// mentions reports whether a method names the given identifier, or the
	// given selector when the name contains a dot.
	mentions := func(method, want string) bool {
		body, ok := bodies[method]
		if !ok {
			t.Fatalf("%s not found in room_state_resource.go", method)
		}
		var found bool
		ast.Inspect(body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.Ident:
				if node.Name == want {
					found = true
				}
			case *ast.SelectorExpr:
				if inner, ok := node.X.(*ast.SelectorExpr); ok &&
					inner.Sel.Name+"."+node.Sel.Name == want {
					found = true
				}
			}
			return true
		})
		return found
	}

	cases := []struct{ method, want, why string }{
		{"ModifyPlan", "Raw.IsNull", "a resource with an owned event type could no longer be destroyed"},
		{"ModifyPlan", "roomStateOwnershipDiag", "two resources could own one event again"},
		{"Delete", "clearingLocksRoom", "a destroy could clear m.room.power_levels and lock the room"},
	}
	for _, c := range cases {
		if !mentions(c.method, c.want) {
			t.Errorf("%s no longer refers to %s, so %s (issue #58)", c.method, c.want, c.why)
		}
	}
}
