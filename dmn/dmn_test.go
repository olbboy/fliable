package dmn

import (
	"strings"
	"testing"

	"github.com/olbboy/fliable/engine"
	"github.com/olbboy/fliable/store"
)

func discountTable() *Decision {
	return &Decision{
		ID:        "discount",
		Name:      "Order discount",
		HitPolicy: HitFirst,
		Inputs: []Input{
			{Label: "Tier", Expression: "customer.tier"},
			{Label: "Amount", Expression: "amount"},
		},
		Outputs: []Output{{Name: "discount"}},
		Rules: []Rule{
			{InputEntries: []string{`"gold"`, ">= 1000"}, OutputEntries: []string{"0.2"}},
			{InputEntries: []string{`"gold"`, "-"}, OutputEntries: []string{"0.1"}},
			{InputEntries: []string{`"silver","bronze"`, "[100..1000)"}, OutputEntries: []string{"0.05"}},
			{InputEntries: []string{"-", "-"}, OutputEntries: []string{"0"}},
		},
	}
}

func TestFirstHitPolicy(t *testing.T) {
	d := discountTable()
	cases := []struct {
		tier   string
		amount float64
		want   float64
	}{
		{"gold", 2000, 0.2},
		{"gold", 500, 0.1},
		{"silver", 500, 0.05},
		{"bronze", 100, 0.05},
		{"silver", 50, 0},
		{"none", 99999, 0},
	}
	for _, tc := range cases {
		got, err := d.Evaluate(map[string]any{
			"customer": map[string]any{"tier": tc.tier},
			"amount":   tc.amount,
		})
		if err != nil {
			t.Fatalf("%s/%v: %v", tc.tier, tc.amount, err)
		}
		if got != tc.want {
			t.Errorf("%s/%v = %v, want %v", tc.tier, tc.amount, got, tc.want)
		}
	}
}

func TestUniqueViolation(t *testing.T) {
	d := &Decision{
		ID: "u", HitPolicy: HitUnique,
		Inputs:  []Input{{Expression: "x"}},
		Outputs: []Output{{Name: "y"}},
		Rules: []Rule{
			{InputEntries: []string{"> 0"}, OutputEntries: []string{"1"}},
			{InputEntries: []string{"> 10"}, OutputEntries: []string{"2"}},
		},
	}
	if _, err := d.Evaluate(map[string]any{"x": 5}); err != nil {
		t.Errorf("single match should pass: %v", err)
	}
	if _, err := d.Evaluate(map[string]any{"x": 50}); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Errorf("want UNIQUE violation, got %v", err)
	}
	// No match: nil result, no error.
	v, err := d.Evaluate(map[string]any{"x": -1})
	if err != nil || v != nil {
		t.Errorf("no match = %v, %v", v, err)
	}
}

func TestAnyPolicy(t *testing.T) {
	d := &Decision{
		ID: "a", HitPolicy: HitAny,
		Inputs:  []Input{{Expression: "x"}},
		Outputs: []Output{{Name: "y"}},
		Rules: []Rule{
			{InputEntries: []string{"> 0"}, OutputEntries: []string{`"pos"`}},
			{InputEntries: []string{"> 10"}, OutputEntries: []string{`"pos"`}},
		},
	}
	v, err := d.Evaluate(map[string]any{"x": 50})
	if err != nil || v != "pos" {
		t.Errorf("ANY agree = %v, %v", v, err)
	}
	d.Rules[1].OutputEntries = []string{`"neg"`}
	if _, err := d.Evaluate(map[string]any{"x": 50}); err == nil {
		t.Error("ANY disagreement should error")
	}
}

func TestPriorityPolicy(t *testing.T) {
	d := &Decision{
		ID: "p", HitPolicy: HitPriority,
		Inputs:  []Input{{Expression: "score"}},
		Outputs: []Output{{Name: "risk", Values: []string{`"high"`, `"medium"`, `"low"`}}},
		Rules: []Rule{
			{InputEntries: []string{"< 700"}, OutputEntries: []string{`"medium"`}},
			{InputEntries: []string{"< 500"}, OutputEntries: []string{`"high"`}},
			{InputEntries: []string{"-"}, OutputEntries: []string{`"low"`}},
		},
	}
	v, err := d.Evaluate(map[string]any{"score": 400})
	if err != nil || v != "high" {
		t.Errorf("priority = %v, %v", v, err)
	}
	v, _ = d.Evaluate(map[string]any{"score": 600})
	if v != "medium" {
		t.Errorf("priority mid = %v", v)
	}
	v, _ = d.Evaluate(map[string]any{"score": 800})
	if v != "low" {
		t.Errorf("priority low = %v", v)
	}
}

func TestCollectPolicy(t *testing.T) {
	d := &Decision{
		ID: "c", HitPolicy: HitCollect, Aggregation: AggSum,
		Inputs:  []Input{{Expression: "order.total"}},
		Outputs: []Output{{Name: "fee"}},
		Rules: []Rule{
			{InputEntries: []string{"> 0"}, OutputEntries: []string{"5"}},
			{InputEntries: []string{"> 100"}, OutputEntries: []string{"10"}},
			{InputEntries: []string{"> 1000"}, OutputEntries: []string{"20"}},
		},
	}
	v, err := d.Evaluate(map[string]any{"order": map[string]any{"total": 500}})
	if err != nil || v != 15.0 {
		t.Errorf("COLLECT SUM = %v, %v", v, err)
	}

	d.Aggregation = AggCount
	v, _ = d.Evaluate(map[string]any{"order": map[string]any{"total": 5000}})
	if v != 3.0 {
		t.Errorf("COLLECT COUNT = %v", v)
	}

	d.Aggregation = AggNone
	v, _ = d.Evaluate(map[string]any{"order": map[string]any{"total": 500}})
	list, ok := v.([]any)
	if !ok || len(list) != 2 {
		t.Errorf("COLLECT list = %v", v)
	}

	d.Aggregation = AggMax
	v, _ = d.Evaluate(map[string]any{"order": map[string]any{"total": 500}})
	if v != 10.0 {
		t.Errorf("COLLECT MAX = %v", v)
	}
}

func TestUnaryTests(t *testing.T) {
	cases := []struct {
		cell  string
		input any
		want  bool
	}{
		{"-", "anything", true},
		{"", 5, true},
		{"< 10", 5, true},
		{"< 10", 15, false},
		{">= 10", 10, true},
		{"!= 3", 4, true},
		{"[1..10]", 10, true},
		{"[1..10)", 10, false},
		{"(0..5]", 0, false},
		{`"gold"`, "gold", true},
		{`"gold"`, "silver", false},
		{`"gold","silver"`, "silver", true},
		{"not(< 10)", 15, true},
		{"? > limit", 50, true}, // limit=10 in vars
		{"? > limit", 5, false},
		{"true", true, true},
		{"true", false, false},
		{"1,2,3", 2, true},
		{"1,2,3", 5, false},
	}
	vars := map[string]any{"limit": 10}
	for _, tc := range cases {
		got, err := matchUnaryTest(tc.cell, tc.input, vars)
		if err != nil {
			t.Errorf("matchUnaryTest(%q, %v): %v", tc.cell, tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("matchUnaryTest(%q, %v) = %v, want %v", tc.cell, tc.input, got, tc.want)
		}
	}
}

const dmnXML = `<?xml version="1.0" encoding="UTF-8"?>
<definitions xmlns="https://www.omg.org/spec/DMN/20191111/MODEL/" id="defs" name="risk">
  <decision id="riskLevel" name="Risk level">
    <decisionTable hitPolicy="FIRST">
      <input label="Credit score">
        <inputExpression typeRef="number"><text>creditScore</text></inputExpression>
      </input>
      <input label="Region">
        <inputExpression typeRef="string"><text>region</text></inputExpression>
      </input>
      <output name="risk" label="Risk"/>
      <rule>
        <inputEntry><text>&lt; 500</text></inputEntry>
        <inputEntry><text>-</text></inputEntry>
        <outputEntry><text>"high"</text></outputEntry>
      </rule>
      <rule>
        <inputEntry><text>[500..700)</text></inputEntry>
        <inputEntry><text>"EU","US"</text></inputEntry>
        <outputEntry><text>"medium"</text></outputEntry>
      </rule>
      <rule>
        <inputEntry><text>-</text></inputEntry>
        <inputEntry><text>-</text></inputEntry>
        <outputEntry><text>"low"</text></outputEntry>
      </rule>
    </decisionTable>
  </decision>
</definitions>`

func TestParseXMLAndEvaluate(t *testing.T) {
	reg := NewRegistry()
	ds, err := reg.RegisterXML([]byte(dmnXML))
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].ID != "riskLevel" {
		t.Fatalf("parsed = %+v", ds)
	}
	v, err := reg.EvaluateDecision("riskLevel", map[string]any{"creditScore": 450, "region": "EU"})
	if err != nil || v != "high" {
		t.Errorf("high = %v, %v", v, err)
	}
	v, _ = reg.EvaluateDecision("riskLevel", map[string]any{"creditScore": 600, "region": "US"})
	if v != "medium" {
		t.Errorf("medium = %v", v)
	}
	v, _ = reg.EvaluateDecision("riskLevel", map[string]any{"creditScore": 600, "region": "APAC"})
	if v != "low" {
		t.Errorf("low = %v", v)
	}
}

// TestBusinessRuleTaskIntegration wires the registry into a process.
func TestBusinessRuleTaskIntegration(t *testing.T) {
	reg := NewRegistry()
	if _, err := reg.RegisterXML([]byte(dmnXML)); err != nil {
		t.Fatal(err)
	}
	e := engine.New(store.NewMemory(), engine.WithDecisionEvaluator(reg))
	_, err := e.Deploy([]byte(`<?xml version="1.0"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             xmlns:fliable="https://fliable.dev/schema/1.0" targetNamespace="t">
  <process id="p" isExecutable="true">
    <startEvent id="s"/>
    <sequenceFlow id="f1" sourceRef="s" targetRef="rate"/>
    <businessRuleTask id="rate" fliable:decisionRef="riskLevel" fliable:resultVariable="risk"/>
    <sequenceFlow id="f2" sourceRef="rate" targetRef="e"/>
    <endEvent id="e"/>
  </process>
</definitions>`), "")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := e.StartInstance("p", "", map[string]any{"creditScore": 450, "region": "EU"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != store.InstanceCompleted || inst.Variables["risk"] != "high" {
		t.Errorf("instance = %s, risk = %v", inst.State, inst.Variables["risk"])
	}
}
