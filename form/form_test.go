package form

import (
	"strings"
	"testing"

	"github.com/olbboy/fliable/store"
)

const approveForm = `{
  "key": "approve",
  "name": "Approval",
  "fields": [
    {"id": "amount",   "type": "number",  "required": true, "min": 0, "max": 10000},
    {"id": "reason",   "type": "string",  "min": 3, "max": 200},
    {"id": "approved", "type": "boolean", "required": true},
    {"id": "tier",     "type": "enum",    "options": ["gold", "silver"], "default": "silver"},
    {"id": "email",    "type": "string",  "pattern": "^[^@]+@[^@]+$"},
    {"id": "escalateTo", "type": "string", "required": true, "visibleIf": "amount > 5000"}
  ]
}`

func reg(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(store.NewMemory())
	if _, err := r.Deploy([]byte(approveForm)); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestFormValidation(t *testing.T) {
	r := reg(t)
	cases := []struct {
		name string
		vars map[string]any
		want string // substring of the error, "" = valid
	}{
		{"valid", map[string]any{"amount": 100.0, "approved": true}, ""},
		{"missing required", map[string]any{"amount": 100.0}, `"approved" is required`},
		{"wrong type", map[string]any{"amount": "lots", "approved": true}, `"amount" must be a number`},
		{"below min", map[string]any{"amount": -5.0, "approved": true}, "below minimum"},
		{"bad enum", map[string]any{"amount": 1.0, "approved": true, "tier": "bronze"}, "must be one of"},
		{"bad pattern", map[string]any{"amount": 1.0, "approved": true, "email": "nope"}, "does not match"},
		{"short string", map[string]any{"amount": 1.0, "approved": true, "reason": "no"}, "shorter than"},
		{"conditional hidden", map[string]any{"amount": 100.0, "approved": false}, ""},
		{"conditional required", map[string]any{"amount": 9000.0, "approved": false}, `"escalateTo" is required`},
		{"conditional satisfied", map[string]any{"amount": 9000.0, "approved": false, "escalateTo": "cfo"}, ""},
	}
	for _, tc := range cases {
		err := r.Validate("approve", tc.vars)
		if tc.want == "" && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestFormDefaultsApplied(t *testing.T) {
	r := reg(t)
	vars := map[string]any{"amount": 10.0, "approved": true}
	if err := r.Validate("approve", vars); err != nil {
		t.Fatal(err)
	}
	if vars["tier"] != "silver" {
		t.Fatalf("default not applied: %v", vars["tier"])
	}
}

func TestUnknownFormKeyIsPermissive(t *testing.T) {
	r := NewRegistry(store.NewMemory())
	if err := r.Validate("external-form", map[string]any{"x": 1}); err != nil {
		t.Fatalf("unknown key must validate trivially: %v", err)
	}
}

func TestDeployRejectsBadDefinitions(t *testing.T) {
	r := NewRegistry(store.NewMemory())
	bad := []string{
		`{"fields": []}`, // no key
		`{"key": "a", "fields": [{"id": "x"}]}`,                             // no type
		`{"key": "a", "fields": [{"id": "x", "type": "blob"}]}`,             // bad type
		`{"key": "a", "fields": [{"id": "x", "type": "enum"}]}`,             // enum w/o options
		`{"key": "a", "fields": [{"id":"x","type":"string","pattern":"["}]}`, // bad regexp
		`{"key": "a", "fields": [{"id":"x","type":"string"},{"id":"x","type":"string"}]}`, // dup
	}
	for _, doc := range bad {
		if _, err := r.Deploy([]byte(doc)); err == nil {
			t.Errorf("accepted bad definition: %s", doc)
		}
	}
}

func TestRegistrySurvivesRestart(t *testing.T) {
	st := store.NewMemory()
	r := NewRegistry(st)
	if _, err := r.Deploy([]byte(approveForm)); err != nil {
		t.Fatal(err)
	}
	// A fresh registry over the same store lazily reloads from blobs.
	r2 := NewRegistry(st)
	def, err := r2.Get("approve")
	if err != nil || len(def.Fields) != 6 {
		t.Fatalf("reload: %+v %v", def, err)
	}
}
