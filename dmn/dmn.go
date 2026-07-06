// Package dmn implements DMN-style decision tables for Fliable business
// rule tasks: five hit policies, COLLECT aggregations, FEEL-flavored
// unary tests evaluated by the sandboxed Fliable expression language,
// loadable from DMN XML or built programmatically in Go.
package dmn

import (
	"fmt"
	"sync"

	"github.com/olbboy/fliable/expr"
)

// HitPolicy controls how multiple matching rules combine.
type HitPolicy string

// Supported hit policies.
const (
	HitUnique   HitPolicy = "UNIQUE"   // at most one rule may match
	HitFirst    HitPolicy = "FIRST"    // first match wins
	HitAny      HitPolicy = "ANY"      // all matches must agree
	HitPriority HitPolicy = "PRIORITY" // highest-priority output value wins
	HitCollect  HitPolicy = "COLLECT"  // gather all matches (optionally aggregate)
)

// Aggregation folds COLLECT results.
type Aggregation string

// COLLECT aggregations.
const (
	AggNone  Aggregation = ""
	AggSum   Aggregation = "SUM"
	AggMin   Aggregation = "MIN"
	AggMax   Aggregation = "MAX"
	AggCount Aggregation = "COUNT"
)

// Input is one input column: an expression evaluated against the
// decision context.
type Input struct {
	Label      string
	Expression string
}

// Output is one output column. Values (optional) enumerate allowed
// values in priority order for the PRIORITY hit policy.
type Output struct {
	Name   string
	Label  string
	Values []string
}

// Rule is one row: unary tests per input column and expressions per
// output column.
type Rule struct {
	InputEntries  []string
	OutputEntries []string
	Description   string
}

// Decision is a decision table.
type Decision struct {
	ID          string
	Name        string
	HitPolicy   HitPolicy
	Aggregation Aggregation
	Inputs      []Input
	Outputs     []Output
	Rules       []Rule
}

// Registry holds deployed decisions and implements the engine's
// DecisionEvaluator.
type Registry struct {
	mu        sync.RWMutex
	decisions map[string]*Decision
}

// NewRegistry creates an empty decision registry.
func NewRegistry() *Registry {
	return &Registry{decisions: map[string]*Decision{}}
}

// Register adds or replaces a decision after validating it.
func (r *Registry) Register(d *Decision) error {
	if err := d.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.decisions[d.ID] = d
	return nil
}

// Get returns a decision by id.
func (r *Registry) Get(id string) (*Decision, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.decisions[id]
	return d, ok
}

// List returns all registered decision IDs.
func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.decisions))
	for id := range r.decisions {
		out = append(out, id)
	}
	return out
}

// EvaluateDecision implements engine.DecisionEvaluator.
func (r *Registry) EvaluateDecision(key string, vars map[string]any) (any, error) {
	d, ok := r.Get(key)
	if !ok {
		return nil, fmt.Errorf("dmn: unknown decision %q", key)
	}
	return d.Evaluate(vars)
}

// Validate checks structural soundness.
func (d *Decision) Validate() error {
	if d.ID == "" {
		return fmt.Errorf("dmn: decision without id")
	}
	switch d.HitPolicy {
	case HitUnique, HitFirst, HitAny, HitPriority, HitCollect, "":
	default:
		return fmt.Errorf("dmn: decision %s: unsupported hit policy %q", d.ID, d.HitPolicy)
	}
	if len(d.Inputs) == 0 {
		return fmt.Errorf("dmn: decision %s has no inputs", d.ID)
	}
	if len(d.Outputs) == 0 {
		return fmt.Errorf("dmn: decision %s has no outputs", d.ID)
	}
	for i, rule := range d.Rules {
		if len(rule.InputEntries) != len(d.Inputs) {
			return fmt.Errorf("dmn: decision %s rule %d has %d input entries, want %d", d.ID, i+1, len(rule.InputEntries), len(d.Inputs))
		}
		if len(rule.OutputEntries) != len(d.Outputs) {
			return fmt.Errorf("dmn: decision %s rule %d has %d output entries, want %d", d.ID, i+1, len(rule.OutputEntries), len(d.Outputs))
		}
	}
	if d.HitPolicy == HitPriority {
		for _, o := range d.Outputs {
			if len(o.Values) == 0 {
				return fmt.Errorf("dmn: decision %s: PRIORITY needs allowed values on output %q", d.ID, o.Name)
			}
		}
	}
	return nil
}

// Evaluate runs the table. Single-output tables return the scalar output
// value (COLLECT: a list); multi-output tables return map[string]any
// (COLLECT: a list of maps). No match returns nil.
func (d *Decision) Evaluate(vars map[string]any) (any, error) {
	policy := d.HitPolicy
	if policy == "" {
		policy = HitUnique
	}

	// Evaluate input column expressions once.
	inputVals := make([]any, len(d.Inputs))
	for i, in := range d.Inputs {
		v, err := expr.Eval(in.Expression, vars)
		if err != nil {
			return nil, fmt.Errorf("dmn: decision %s input %d (%s): %w", d.ID, i+1, in.Label, err)
		}
		inputVals[i] = v
	}

	var matches []map[string]any
	var matchedRules []int
	for ri, rule := range d.Rules {
		ok, err := d.ruleMatches(rule, inputVals, vars)
		if err != nil {
			return nil, fmt.Errorf("dmn: decision %s rule %d: %w", d.ID, ri+1, err)
		}
		if !ok {
			continue
		}
		out, err := d.ruleOutputs(rule, vars)
		if err != nil {
			return nil, fmt.Errorf("dmn: decision %s rule %d: %w", d.ID, ri+1, err)
		}
		matches = append(matches, out)
		matchedRules = append(matchedRules, ri+1)
		if policy == HitFirst {
			break
		}
	}
	if len(matches) == 0 {
		return nil, nil
	}

	switch policy {
	case HitUnique:
		if len(matches) > 1 {
			return nil, fmt.Errorf("dmn: decision %s: UNIQUE violated, rules %v all matched", d.ID, matchedRules)
		}
		return d.scalarize(matches[0]), nil
	case HitFirst:
		return d.scalarize(matches[0]), nil
	case HitAny:
		for _, m := range matches[1:] {
			for k, v := range matches[0] {
				if expr.Stringify(m[k]) != expr.Stringify(v) {
					return nil, fmt.Errorf("dmn: decision %s: ANY violated, rules %v disagree on %q", d.ID, matchedRules, k)
				}
			}
		}
		return d.scalarize(matches[0]), nil
	case HitPriority:
		best := matches[0]
		bestRank := d.priorityRank(matches[0])
		for _, m := range matches[1:] {
			if r := d.priorityRank(m); r < bestRank {
				best, bestRank = m, r
			}
		}
		return d.scalarize(best), nil
	case HitCollect:
		return d.collect(matches)
	}
	return nil, fmt.Errorf("dmn: decision %s: unsupported hit policy %q", d.ID, policy)
}

func (d *Decision) ruleMatches(rule Rule, inputVals []any, vars map[string]any) (bool, error) {
	for i, entry := range rule.InputEntries {
		ok, err := matchUnaryTest(entry, inputVals[i], vars)
		if err != nil {
			return false, fmt.Errorf("input %d (%s): %w", i+1, d.Inputs[i].Label, err)
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func (d *Decision) ruleOutputs(rule Rule, vars map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(d.Outputs))
	for i, oe := range rule.OutputEntries {
		if oe == "" || oe == "-" {
			out[d.Outputs[i].Name] = nil
			continue
		}
		v, err := expr.Eval(oe, vars)
		if err != nil {
			return nil, fmt.Errorf("output %q: %w", d.Outputs[i].Name, err)
		}
		out[d.Outputs[i].Name] = v
	}
	return out, nil
}

// scalarize unwraps single-column results.
func (d *Decision) scalarize(m map[string]any) any {
	if len(d.Outputs) == 1 {
		return m[d.Outputs[0].Name]
	}
	return m
}

// priorityRank returns the position of the first output's value in its
// allowed-values list (lower = higher priority).
func (d *Decision) priorityRank(m map[string]any) int {
	o := d.Outputs[0]
	got := expr.Stringify(m[o.Name])
	for i, allowed := range o.Values {
		if allowedValueString(allowed) == got {
			return i
		}
	}
	return len(o.Values)
}

// allowedValueString normalizes an allowed value entry (may be a quoted
// FEEL string literal).
func allowedValueString(s string) string {
	if v, err := expr.Eval(s, nil); err == nil {
		return expr.Stringify(v)
	}
	return s
}

func (d *Decision) collect(matches []map[string]any) (any, error) {
	if d.Aggregation == AggCount {
		return float64(len(matches)), nil
	}
	if d.Aggregation != AggNone {
		if len(d.Outputs) != 1 {
			return nil, fmt.Errorf("dmn: decision %s: COLLECT aggregation needs exactly one output", d.ID)
		}
		var acc float64
		for i, m := range matches {
			v := m[d.Outputs[0].Name]
			f, ok := v.(float64)
			if !ok {
				return nil, fmt.Errorf("dmn: decision %s: COLLECT %s on non-numeric output %v", d.ID, d.Aggregation, v)
			}
			switch d.Aggregation {
			case AggSum:
				acc += f
			case AggMin:
				if i == 0 || f < acc {
					acc = f
				}
			case AggMax:
				if i == 0 || f > acc {
					acc = f
				}
			default:
				return nil, fmt.Errorf("dmn: decision %s: unsupported aggregation %q", d.ID, d.Aggregation)
			}
		}
		return acc, nil
	}
	out := make([]any, len(matches))
	for i, m := range matches {
		out[i] = d.scalarize(m)
	}
	return out, nil
}
