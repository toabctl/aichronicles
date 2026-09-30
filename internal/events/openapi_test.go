package events

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// openAPISchema is the slice of an OpenAPI schema object this test
// reads.
type openAPISchema struct {
	Required   []string                 `yaml:"required"`
	Properties map[string]openAPISchema `yaml:"properties"`
	Enum       []string                 `yaml:"enum"`
}

func loadOpenAPISchemas(t *testing.T) map[string]openAPISchema {
	t.Helper()
	b, err := os.ReadFile("../../api/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]openAPISchema `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse api/openapi.yaml: %v", err)
	}
	return doc.Components.Schemas
}

// jsonFields returns the JSON field names of struct type v.
func jsonFields(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := range rt.NumField() {
		name, _, _ := strings.Cut(rt.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func sortedKeys(m map[string]openAPISchema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestOpenAPIMatchesTheGoTypes keeps api/openapi.yaml — the contract
// third-party agents build against — in lockstep with what the daemon
// actually decodes and validates. The spec had drifted unnoticed (a
// missing `subagent` under additionalProperties:false, a `redaction`
// requirement the daemon never enforced) because nothing read it.
func TestOpenAPIMatchesTheGoTypes(t *testing.T) {
	t.Parallel()
	schemas := loadOpenAPISchemas(t)
	for name, v := range map[string]any{
		"Envelope": Envelope{}, "Tool": Tool{}, "Subagent": Subagent{},
		"Redaction": Redaction{}, "Ack": Ack{},
	} {
		s, ok := schemas[name]
		if !ok {
			t.Errorf("schema %s missing from api/openapi.yaml", name)
			continue
		}
		if got, want := sortedKeys(s.Properties), jsonFields(v); !slices.Equal(got, want) {
			t.Errorf("%s properties:\n spec %v\n Go   %v", name, got, want)
		}
	}

	env := schemas["Envelope"]
	// Exactly the fields Validate rejects when absent.
	required := slices.Sorted(slices.Values(env.Required))
	if want := []string{"event_id", "kind", "payload", "source_agent", "source_session_id", "ts_source", "v"}; !slices.Equal(required, want) {
		t.Errorf("Envelope required: spec %v, Validate enforces %v", required, want)
	}
	kinds := env.Properties["kind"].Enum
	if len(kinds) != len(validKinds) {
		t.Errorf("kind enum has %d values, validKinds %d", len(kinds), len(validKinds))
	}
	for _, k := range kinds {
		if !IsValidKind(k) {
			t.Errorf("spec kind %q is not a valid kind", k)
		}
	}
	for _, r := range env.Properties["role"].Enum {
		if !IsValidRole(r) {
			t.Errorf("spec role %q is not a valid role", r)
		}
	}
	if got := slices.Sorted(slices.Values(env.Properties["transport"].Enum)); !slices.Equal(got, []string{TransportHook, TransportImport}) {
		t.Errorf("transport enum: spec %v, Validate accepts [%s %s]", got, TransportHook, TransportImport)
	}
}
