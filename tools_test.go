package main

import (
	json "encoding/json/v2"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// Every tool needs a schema and an implementation, and every tool needs a tier:
// a name in one list and not the others is a tool that either cannot be called
// or can never be turned off.
func TestToolsSchemasHandlersAndTiersAgree(t *testing.T) {
	specs, err := loadToolSpecs()
	if err != nil {
		t.Fatal(err)
	}

	schemaNames := map[string]bool{}
	for _, spec := range specs {
		schemaNames[spec.Name] = true
		if _, ok := handlers[spec.Name]; !ok {
			t.Errorf("%s has a schema but no handler", spec.Name)
		}
		if !allTools[spec.Name] {
			t.Errorf("%s has a schema but is in no tier", spec.Name)
		}
	}
	for name := range handlers {
		if !schemaNames[name] {
			t.Errorf("%s has a handler but no schema", name)
		}
	}
	for name := range allTools {
		if !schemaNames[name] {
			t.Errorf("%s is in a tier but has no schema", name)
		}
	}
}

func TestToolSchemasAreValidAndDescribed(t *testing.T) {
	specs, err := loadToolSpecs()
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range specs {
		t.Run(spec.Name, func(t *testing.T) {
			if spec.Description == "" {
				t.Error("description is empty; the model has nothing to select on")
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(spec.InputSchema, &schema); err != nil {
				t.Fatalf("parsing schema: %v", err)
			}
			if _, err := schema.Resolve(nil); err != nil {
				t.Fatalf("resolving schema: %v", err)
			}
			if schema.Type != "object" {
				t.Errorf("input schema type = %q, want object", schema.Type)
			}
		})
	}
}

// Every tool is assigned a concurrency category, either explicitly or by
// falling back to the concurrent query pool. The desktop tools are the ones
// that matter: two of those at once fight over one mouse.
func TestDesktopToolsAreSerialised(t *testing.T) {
	if categoryLimits[catDesktop] != 1 {
		t.Fatalf("desktop limit = %d, want 1", categoryLimits[catDesktop])
	}
	for _, name := range []string{"Click", "Type", "Snapshot", "ScreenRecord", "Shortcut"} {
		if got := categoryFor(name); got != catDesktop {
			t.Errorf("%s category = %s, want %s", name, got, catDesktop)
		}
	}
}
