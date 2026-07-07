// Package form is Fliable's schema-driven form engine: JSON form
// definitions bound to user tasks (and start events) by form key, with
// server-side validation on completion. Rendering is deliberately not
// here — forms are data; the headless SDK (or any client) renders them
// with its own components. This mirrors Fliable's headless-first UI
// strategy: one schema, any component library.
package form

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/olbboy/fliable/expr"
	"github.com/olbboy/fliable/store"
)

// BlobKind is the store blob namespace for form definitions.
const BlobKind = "form"

// ErrNotFound is returned when a form key is unknown.
var ErrNotFound = errors.New("form: not found")

// Definition is one deployable form.
type Definition struct {
	Key    string  `json:"key"`
	Name   string  `json:"name,omitempty"`
	Fields []Field `json:"fields"`
}

// Field is one input in a form.
type Field struct {
	ID       string `json:"id"`
	Label    string `json:"label,omitempty"`
	Type     string `json:"type"` // string, text, number, boolean, enum, date
	Required bool   `json:"required,omitempty"`
	// Options constrain enum fields.
	Options []string `json:"options,omitempty"`
	// Min/Max bound numbers (value) and strings (length).
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
	// Pattern is an RE2 regexp a string value must match.
	Pattern string `json:"pattern,omitempty"`
	// Default is applied when the value is absent and the field visible.
	Default any `json:"default,omitempty"`
	// VisibleIf is a Fliable expression over the submitted variables;
	// false hides the field (hidden fields skip required/type checks).
	VisibleIf string `json:"visibleIf,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Registry stores form definitions and validates submissions. Deployed
// forms persist through the store's blob records, so they survive
// restarts like every other engine record.
type Registry struct {
	st store.Store

	mu    sync.RWMutex
	cache map[string]*Definition
}

// NewRegistry creates a form registry over the store.
func NewRegistry(st store.Store) *Registry {
	return &Registry{st: st, cache: map[string]*Definition{}}
}

// Deploy validates and stores a form definition (JSON).
func (r *Registry) Deploy(doc []byte) (*Definition, error) {
	var def Definition
	dec := json.NewDecoder(strings.NewReader(string(doc)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&def); err != nil {
		return nil, fmt.Errorf("form: invalid definition: %w", err)
	}
	if def.Key == "" {
		return nil, errors.New("form: definition needs a key")
	}
	seen := map[string]bool{}
	for i, f := range def.Fields {
		if f.ID == "" {
			return nil, fmt.Errorf("form: field %d has no id", i)
		}
		if seen[f.ID] {
			return nil, fmt.Errorf("form: duplicate field %q", f.ID)
		}
		seen[f.ID] = true
		switch f.Type {
		case "string", "text", "number", "boolean", "enum", "date":
		case "":
			return nil, fmt.Errorf("form: field %q has no type", f.ID)
		default:
			return nil, fmt.Errorf("form: field %q has unknown type %q", f.ID, f.Type)
		}
		if f.Type == "enum" && len(f.Options) == 0 {
			return nil, fmt.Errorf("form: enum field %q needs options", f.ID)
		}
		if f.Pattern != "" {
			if _, err := regexp.Compile(f.Pattern); err != nil {
				return nil, fmt.Errorf("form: field %q pattern: %w", f.ID, err)
			}
		}
	}
	if err := r.st.PutBlob(&store.Blob{Kind: BlobKind, Key: def.Key, Data: doc, UpdatedAt: time.Now().UTC()}); err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.cache[def.Key] = &def
	r.mu.Unlock()
	return &def, nil
}

// Get returns a form definition by key.
func (r *Registry) Get(key string) (*Definition, error) {
	r.mu.RLock()
	if d, ok := r.cache[key]; ok {
		r.mu.RUnlock()
		return d, nil
	}
	r.mu.RUnlock()
	b, err := r.st.GetBlob(BlobKind, key)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var def Definition
	if err := json.Unmarshal(b.Data, &def); err != nil {
		return nil, fmt.Errorf("form: stored definition %q is corrupt: %w", key, err)
	}
	r.mu.Lock()
	r.cache[key] = &def
	r.mu.Unlock()
	return &def, nil
}

// List returns all deployed forms.
func (r *Registry) List() ([]*Definition, error) {
	blobs, err := r.st.ListBlobs(BlobKind)
	if err != nil {
		return nil, err
	}
	out := make([]*Definition, 0, len(blobs))
	for _, b := range blobs {
		var def Definition
		if err := json.Unmarshal(b.Data, &def); err != nil {
			continue
		}
		out = append(out, &def)
	}
	return out, nil
}

// Delete removes a form.
func (r *Registry) Delete(key string) error {
	r.mu.Lock()
	delete(r.cache, key)
	r.mu.Unlock()
	return r.st.DeleteBlob(BlobKind, key)
}

// Validate checks a submission against the form and applies defaults in
// place. An unknown form key validates trivially — a task may carry a
// formKey rendered by an external form service. It implements
// engine.FormValidator.
func (r *Registry) Validate(formKey string, vars map[string]any) error {
	def, err := r.Get(formKey)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var problems []string
	for _, f := range def.Fields {
		if f.VisibleIf != "" {
			v, err := expr.Eval(f.VisibleIf, vars)
			if err != nil || !expr.Truthy(v) {
				continue // hidden: no checks, no default
			}
		}
		val, present := vars[f.ID]
		if !present || val == nil {
			if f.Default != nil && vars != nil {
				vars[f.ID] = f.Default
				continue
			}
			if f.Required {
				problems = append(problems, fmt.Sprintf("field %q is required", f.ID))
			}
			continue
		}
		if f.ReadOnly {
			problems = append(problems, fmt.Sprintf("field %q is read-only", f.ID))
			continue
		}
		if p := checkField(f, val); p != "" {
			problems = append(problems, p)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("form: %s", strings.Join(problems, "; "))
	}
	return nil
}

func checkField(f Field, val any) string {
	switch f.Type {
	case "string", "text", "date":
		s, ok := val.(string)
		if !ok {
			return fmt.Sprintf("field %q must be a string", f.ID)
		}
		if f.Min != nil && float64(len(s)) < *f.Min {
			return fmt.Sprintf("field %q shorter than %v", f.ID, *f.Min)
		}
		if f.Max != nil && float64(len(s)) > *f.Max {
			return fmt.Sprintf("field %q longer than %v", f.ID, *f.Max)
		}
		if f.Pattern != "" {
			if re, err := regexp.Compile(f.Pattern); err == nil && !re.MatchString(s) {
				return fmt.Sprintf("field %q does not match %s", f.ID, f.Pattern)
			}
		}
		if f.Type == "date" {
			if _, err := time.Parse(time.RFC3339, s); err != nil {
				if _, err := time.Parse("2006-01-02", s); err != nil {
					return fmt.Sprintf("field %q is not a date", f.ID)
				}
			}
		}
	case "number":
		n, ok := toFloat(val)
		if !ok {
			return fmt.Sprintf("field %q must be a number", f.ID)
		}
		if f.Min != nil && n < *f.Min {
			return fmt.Sprintf("field %q below minimum %v", f.ID, *f.Min)
		}
		if f.Max != nil && n > *f.Max {
			return fmt.Sprintf("field %q above maximum %v", f.ID, *f.Max)
		}
	case "boolean":
		if _, ok := val.(bool); !ok {
			return fmt.Sprintf("field %q must be a boolean", f.ID)
		}
	case "enum":
		s, ok := val.(string)
		if !ok {
			return fmt.Sprintf("field %q must be one of %v", f.ID, f.Options)
		}
		for _, o := range f.Options {
			if s == o {
				return ""
			}
		}
		return fmt.Sprintf("field %q must be one of %v", f.ID, f.Options)
	}
	return ""
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}
