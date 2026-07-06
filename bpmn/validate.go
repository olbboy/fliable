package bpmn

import (
	"errors"
	"fmt"
)

// ValidationError aggregates all structural problems found in a model so
// modelers can fix everything in one round trip instead of one error at a
// time (an ergonomic improvement over fail-fast validators).
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "bpmn: " + e.Problems[0]
	}
	msg := fmt.Sprintf("bpmn: %d problems found:", len(e.Problems))
	for _, p := range e.Problems {
		msg += "\n  - " + p
	}
	return msg
}

// Validate checks the structural soundness of parsed definitions: flow
// references resolve, executable processes are startable, gateways and
// boundary events are well-formed. It returns a *ValidationError listing
// every problem, or nil.
func Validate(d *Definitions) error {
	var problems []string
	addf := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	if len(d.Processes) == 0 {
		addf("no process defined")
	}
	seen := map[string]bool{}
	for _, p := range d.Processes {
		if p.ID == "" {
			addf("process without id")
			continue
		}
		if seen[p.ID] {
			addf("duplicate process id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Executable {
			validateContainer(&p.Container, "process "+p.ID, true, addf)
		}
	}
	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func validateContainer(c *Container, scope string, isProcess bool, addf func(string, ...any)) {
	if isProcess && len(c.StartEvents()) == 0 {
		addf("%s: no start event", scope)
	}

	for _, id := range c.Order {
		el := c.Elements[id]
		loc := fmt.Sprintf("%s: %s %q", scope, el.Type, el.ID)

		for _, fid := range append(append([]string{}, el.Incoming...), el.Outgoing...) {
			if _, ok := c.Flows[fid]; !ok {
				addf("%s references unknown sequence flow %q", loc, fid)
			}
		}

		switch el.Type {
		case TypeStartEvent:
			if len(el.Incoming) > 0 {
				addf("%s must not have incoming flows", loc)
			}
			if len(el.Outgoing) == 0 {
				addf("%s has no outgoing flow", loc)
			}
		case TypeEndEvent:
			if len(el.Outgoing) > 0 {
				addf("%s must not have outgoing flows", loc)
			}
		case TypeBoundaryEvent:
			att, ok := c.Elements[el.AttachedTo]
			if !ok {
				addf("%s attached to unknown activity %q", loc, el.AttachedTo)
			} else if !att.IsActivity() {
				addf("%s attached to non-activity %q", loc, el.AttachedTo)
			}
			if len(el.Outgoing) == 0 {
				addf("%s has no outgoing flow", loc)
			}
		case TypeExclusiveGateway, TypeInclusiveGateway:
			if el.DefaultFlow != "" {
				if _, ok := c.Flows[el.DefaultFlow]; !ok {
					addf("%s default flow %q not found", loc, el.DefaultFlow)
				}
			}
			if len(el.Outgoing) == 0 {
				addf("%s has no outgoing flow", loc)
			}
		case TypeEventBasedGateway:
			if len(el.Outgoing) < 2 {
				addf("%s needs at least two outgoing flows", loc)
			}
			for _, fid := range el.Outgoing {
				f := c.Flows[fid]
				if f == nil {
					continue
				}
				t := c.Elements[f.TargetRef]
				if t == nil || (t.Type != TypeIntermediateCatchEvent && t.Type != TypeReceiveTask) {
					addf("%s outgoing flow %q must target an intermediate catch event or receive task", loc, fid)
				}
			}
		case TypeParallelGateway:
			if len(el.Outgoing) == 0 {
				addf("%s has no outgoing flow", loc)
			}
		case TypeCallActivity:
			if el.CalledElement == "" {
				addf("%s has no calledElement", loc)
			}
		case TypeBusinessRuleTask:
			if el.DecisionRef == "" {
				addf("%s has no decisionRef", loc)
			}
		case TypeSubProcess:
			if el.Sub == nil || len(el.Sub.Elements) == 0 {
				addf("%s is empty", loc)
			} else {
				if el.TriggeredByEvent {
					ok := false
					for _, s := range el.Sub.StartEvents() {
						if s.Event != nil && s.Event.Kind != KindNone {
							ok = true
						}
					}
					if !ok {
						addf("%s: event sub-process needs a typed start event", loc)
					}
				} else if el.Sub.NoneStartEvent() == nil {
					addf("%s has no none start event", loc)
				}
				validateContainer(el.Sub, scope+"/"+el.ID, false, addf)
			}
		}

		// Every non-end, non-throw node should lead somewhere.
		if len(el.Outgoing) == 0 {
			switch el.Type {
			case TypeEndEvent, TypeIntermediateThrowEvent, TypeBoundaryEvent, TypeStartEvent:
				// already reported or legal
			default:
				addf("%s is a dead end (no outgoing flow)", loc)
			}
		}
	}

	for _, f := range c.Flows {
		if _, ok := c.Elements[f.SourceRef]; !ok {
			addf("%s: sequence flow %q has unknown source %q", scope, f.ID, f.SourceRef)
		}
		if _, ok := c.Elements[f.TargetRef]; !ok {
			addf("%s: sequence flow %q has unknown target %q", scope, f.ID, f.TargetRef)
		}
	}
}

// IsValidationError reports whether err is a *ValidationError.
func IsValidationError(err error) bool {
	var v *ValidationError
	return errors.As(err, &v)
}
