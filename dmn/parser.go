package dmn

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// Parse reads a DMN XML document (the decision table subset) and returns
// its decisions. Standard DMN 1.3+ namespaces and the common modeler
// output shapes are accepted.
func Parse(data []byte) ([]*Decision, error) {
	var root xDMNDefinitions
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("dmn: malformed XML: %w", err)
	}
	if len(root.Decisions) == 0 {
		return nil, fmt.Errorf("dmn: no decisions in document")
	}
	var out []*Decision
	for _, xd := range root.Decisions {
		if xd.Table == nil {
			continue
		}
		d := &Decision{
			ID:          xd.ID,
			Name:        xd.Name,
			HitPolicy:   HitPolicy(strings.ToUpper(orStr(xd.Table.HitPolicy, "UNIQUE"))),
			Aggregation: Aggregation(strings.ToUpper(xd.Table.Aggregation)),
		}
		for _, xi := range xd.Table.Inputs {
			exprText := strings.TrimSpace(xi.Expression.Text)
			d.Inputs = append(d.Inputs, Input{Label: xi.Label, Expression: exprText})
		}
		for _, xo := range xd.Table.Outputs {
			o := Output{Name: orStr(xo.Name, xo.Label), Label: xo.Label}
			if xo.Values != nil {
				for _, v := range splitTop(xo.Values.Text, ',') {
					o.Values = append(o.Values, strings.TrimSpace(v))
				}
			}
			d.Outputs = append(d.Outputs, o)
		}
		for _, xr := range xd.Table.Rules {
			r := Rule{Description: strings.TrimSpace(xr.Description)}
			for _, ie := range xr.InputEntries {
				r.InputEntries = append(r.InputEntries, strings.TrimSpace(ie.Text))
			}
			for _, oe := range xr.OutputEntries {
				r.OutputEntries = append(r.OutputEntries, strings.TrimSpace(oe.Text))
			}
			d.Rules = append(d.Rules, r)
		}
		if err := d.Validate(); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("dmn: document contains no decision tables")
	}
	return out, nil
}

// RegisterXML parses a DMN document and registers every decision table.
func (r *Registry) RegisterXML(data []byte) ([]*Decision, error) {
	ds, err := Parse(data)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if err := r.Register(d); err != nil {
			return nil, err
		}
	}
	return ds, nil
}

func orStr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type xDMNDefinitions struct {
	XMLName   xml.Name    `xml:"definitions"`
	Decisions []xDecision `xml:"decision"`
}

type xDecision struct {
	ID    string  `xml:"id,attr"`
	Name  string  `xml:"name,attr"`
	Table *xTable `xml:"decisionTable"`
}

type xTable struct {
	HitPolicy   string    `xml:"hitPolicy,attr"`
	Aggregation string    `xml:"aggregation,attr"`
	Inputs      []xInput  `xml:"input"`
	Outputs     []xOutput `xml:"output"`
	Rules       []xRule   `xml:"rule"`
}

type xInput struct {
	Label      string `xml:"label,attr"`
	Expression xText  `xml:"inputExpression"`
}

type xOutput struct {
	Name   string `xml:"name,attr"`
	Label  string `xml:"label,attr"`
	Values *xText `xml:"outputValues"`
}

type xRule struct {
	Description   string  `xml:"description"`
	InputEntries  []xText `xml:"inputEntry"`
	OutputEntries []xText `xml:"outputEntry"`
}

type xText struct {
	Text string `xml:"text"`
}
