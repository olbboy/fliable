package rest

import (
	"encoding/json"
	"net/http"
)

// handleOpenAPI serves the machine-readable API description. Any client
// generator (openapi-generator, orval, oazapfts, Kiota, ...) can turn this
// into a typed client for any language or UI framework.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(openAPISpec)
}

// handleDocs serves a self-contained, dependency-free API reference page
// (no external scripts or styles, CSP-safe) that reads /openapi.json.
func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(docsHTML))
}

// openAPISpec is the OpenAPI 3.1 document for the Fliable REST API. It is a
// literal Go value so it stays in lockstep with the routes above and needs
// no build step.
var openAPISpec = map[string]any{
	"openapi": "3.1.0",
	"info": map[string]any{
		"title":       "Fliable API",
		"version":     "1.0.0",
		"description": "Compact, high-efficiency BPMN 2.0 / DMN workflow & BPM platform.",
		"license":     map[string]any{"name": "Apache-2.0"},
	},
	"servers": []any{map[string]any{"url": "/", "description": "This server"}},
	"security": []any{
		map[string]any{"ApiKey": []any{}},
		map[string]any{"Bearer": []any{}},
	},
	"components": map[string]any{
		"securitySchemes": map[string]any{
			"ApiKey": map[string]any{"type": "apiKey", "in": "header", "name": "X-Api-Key"},
			"Bearer": map[string]any{"type": "http", "scheme": "bearer"},
			"Tenant": map[string]any{"type": "apiKey", "in": "header", "name": "X-Tenant-Id"},
		},
		"parameters": map[string]any{
			"limit":  map[string]any{"name": "limit", "in": "query", "schema": map[string]any{"type": "integer", "default": 100, "maximum": 1000}},
			"cursor": map[string]any{"name": "cursor", "in": "query", "schema": map[string]any{"type": "string"}, "description": "nextCursor from the previous page"},
			"desc":   map[string]any{"name": "desc", "in": "query", "schema": map[string]any{"type": "boolean"}},
			"var":    map[string]any{"name": "var", "in": "query", "schema": map[string]any{"type": "array", "items": map[string]any{"type": "string"}}, "description": "Variable predicate name:op:value (op: eq,ne,lt,lte,gt,gte,contains,exists)"},
			"tenant": map[string]any{"name": "X-Tenant-Id", "in": "header", "schema": map[string]any{"type": "string"}},
		},
		"schemas": openAPISchemas,
	},
	"paths": openAPIPaths,
}

var openAPISchemas = map[string]any{
	"Page": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"items":      map[string]any{"type": "array", "items": map[string]any{}},
			"count":      map[string]any{"type": "integer"},
			"nextCursor": map[string]any{"type": "string"},
		},
	},
	"Error": map[string]any{
		"type":       "object",
		"properties": map[string]any{"error": map[string]any{"type": "string"}},
	},
	"Instance": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":            map[string]any{"type": "string"},
			"tenantId":      map[string]any{"type": "string"},
			"definitionId":  map[string]any{"type": "string"},
			"definitionKey": map[string]any{"type": "string"},
			"businessKey":   map[string]any{"type": "string"},
			"state":         map[string]any{"type": "string", "enum": []any{"active", "completed", "terminated"}},
			"suspended":     map[string]any{"type": "boolean"},
			"variables":     map[string]any{"type": "object"},
			"startedAt":     map[string]any{"type": "string", "format": "date-time"},
			"endedAt":       map[string]any{"type": "string", "format": "date-time"},
		},
	},
	"Task": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":              map[string]any{"type": "string"},
			"tenantId":        map[string]any{"type": "string"},
			"instanceId":      map[string]any{"type": "string"},
			"elementId":       map[string]any{"type": "string"},
			"name":            map[string]any{"type": "string"},
			"state":           map[string]any{"type": "string", "enum": []any{"created", "completed", "canceled"}},
			"assignee":        map[string]any{"type": "string"},
			"candidateGroups": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"formKey":         map[string]any{"type": "string"},
			"priority":        map[string]any{"type": "integer"},
			"dueAt":           map[string]any{"type": "string", "format": "date-time"},
		},
	},
	"StartInstance": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"definitionKey": map[string]any{"type": "string"},
			"definitionId":  map[string]any{"type": "string"},
			"businessKey":   map[string]any{"type": "string"},
			"variables":     map[string]any{"type": "object"},
		},
	},
	"CompleteTask": map[string]any{
		"type":       "object",
		"properties": map[string]any{"user": map[string]any{"type": "string"}, "variables": map[string]any{"type": "object"}},
	},
	"Message": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":           map[string]any{"type": "string"},
			"correlationKey": map[string]any{"type": "string"},
			"variables":      map[string]any{"type": "object"},
		},
		"required": []any{"name"},
	},
	"AgentJob": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"id":           map[string]any{"type": "string"},
			"instanceId":   map[string]any{"type": "string"},
			"elementId":    map[string]any{"type": "string"},
			"agent":        map[string]any{"type": "string"},
			"prompt":       map[string]any{"type": "string"},
			"systemPrompt": map[string]any{"type": "string"},
			"tools":        map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"variables":    map[string]any{"type": "object"},
		},
	},
}

func opJSON(summary string, reqSchema, respSchema string, params []any) map[string]any {
	op := map[string]any{"summary": summary}
	if len(params) > 0 {
		op["parameters"] = params
	}
	if reqSchema != "" {
		op["requestBody"] = map[string]any{
			"content": map[string]any{"application/json": map[string]any{
				"schema": map[string]any{"$ref": "#/components/schemas/" + reqSchema},
			}},
		}
	}
	content := map[string]any{}
	if respSchema != "" {
		content["application/json"] = map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/" + respSchema}}
	} else {
		content["application/json"] = map[string]any{"schema": map[string]any{"type": "object"}}
	}
	op["responses"] = map[string]any{
		"200": map[string]any{"description": "OK", "content": content},
		"default": map[string]any{"description": "Error", "content": map[string]any{
			"application/json": map[string]any{"schema": map[string]any{"$ref": "#/components/schemas/Error"}}}},
	}
	return op
}

var listParams = []any{
	map[string]any{"$ref": "#/components/parameters/limit"},
	map[string]any{"$ref": "#/components/parameters/cursor"},
	map[string]any{"$ref": "#/components/parameters/desc"},
	map[string]any{"$ref": "#/components/parameters/var"},
	map[string]any{"$ref": "#/components/parameters/tenant"},
}

var openAPIPaths = map[string]any{
	"/v1/definitions": map[string]any{
		"post": opJSON("Deploy a BPMN definition (raw XML body)", "", "", nil),
		"get":  opJSON("List definitions", "", "", nil),
	},
	"/v1/instances": map[string]any{
		"post": opJSON("Start a process instance", "StartInstance", "Instance", nil),
		"get":  opJSON("Query process instances", "", "Page", listParams),
	},
	"/v1/instances/{id}": map[string]any{
		"get":    opJSON("Get an instance", "", "Instance", pathID()),
		"delete": opJSON("Cancel an instance", "", "", pathID()),
	},
	"/v1/instances/{id}/suspend":   map[string]any{"post": opJSON("Suspend an instance", "", "", pathID())},
	"/v1/instances/{id}/resume":    map[string]any{"post": opJSON("Resume an instance", "", "", pathID())},
	"/v1/instances/{id}/history":   map[string]any{"get": opJSON("Instance history", "", "", pathID())},
	"/v1/instances/{id}/variables": map[string]any{"put": opJSON("Merge variables", "", "", pathID())},
	"/v1/tasks":                    map[string]any{"get": opJSON("Query user tasks", "", "Page", listParams)},
	"/v1/tasks/{id}":               map[string]any{"get": opJSON("Get a task", "", "Task", pathID())},
	"/v1/tasks/{id}/claim":         map[string]any{"post": opJSON("Claim a task", "CompleteTask", "", pathID())},
	"/v1/tasks/{id}/complete":      map[string]any{"post": opJSON("Complete a task", "CompleteTask", "", pathID())},
	"/v1/messages":                 map[string]any{"post": opJSON("Correlate a message", "Message", "", nil)},
	"/v1/signals":                  map[string]any{"post": opJSON("Broadcast a signal", "Message", "", nil)},
	"/v1/incidents":                map[string]any{"get": opJSON("List incidents", "", "", nil)},
	"/v1/incidents/{id}/resolve":   map[string]any{"post": opJSON("Resolve an incident", "", "", pathID())},
	"/v1/batch/{op}": map[string]any{"post": opJSON("Bulk cancel/suspend/resume/resolve-incidents", "", "", []any{
		map[string]any{"name": "op", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}})},
	"/v1/external-tasks/fetch":     map[string]any{"post": opJSON("Fetch & lock external worker tasks", "", "", nil)},
	"/v1/agent-jobs/fetch":         map[string]any{"post": opJSON("Fetch & lock agent jobs for an AI worker", "", "", nil)},
	"/v1/agent-jobs/{id}/complete": map[string]any{"post": opJSON("Complete an agent job with structured output", "", "", pathID())},
	"/v1/agent-jobs/{id}/fail":     map[string]any{"post": opJSON("Fail an agent job", "", "", pathID())},
	"/v1/events":                   map[string]any{"get": map[string]any{"summary": "Server-sent event stream", "responses": map[string]any{"200": map[string]any{"description": "text/event-stream"}}}},
	"/healthz":                     map[string]any{"get": map[string]any{"summary": "Liveness", "responses": map[string]any{"200": map[string]any{"description": "OK"}}}},
	"/metrics":                     map[string]any{"get": map[string]any{"summary": "Prometheus metrics", "responses": map[string]any{"200": map[string]any{"description": "text/plain"}}}},
}

func pathID() []any {
	return []any{map[string]any{"name": "id", "in": "path", "required": true, "schema": map[string]any{"type": "string"}}}
}

const docsHTML = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Fliable API</title>
<style>
:root{color-scheme:light dark}
body{font:15px/1.5 system-ui,sans-serif;margin:0;padding:2rem;max-width:900px;margin-inline:auto}
h1{font-size:1.4rem} .op{display:inline-block;font-weight:700;padding:.1rem .5rem;border-radius:.3rem;color:#fff;font-size:.8rem}
.get{background:#2563eb}.post{background:#16a34a}.put{background:#d97706}.delete{background:#dc2626}
.row{display:flex;gap:.6rem;align-items:baseline;padding:.35rem 0;border-bottom:1px solid #8884}
code{font-family:ui-monospace,monospace}
.muted{opacity:.7}
</style></head><body>
<h1>Fliable API</h1>
<p class="muted">Machine-readable spec at <a href="/openapi.json">/openapi.json</a> — generate a typed client for any framework.</p>
<div id="ops"></div>
<script>
fetch('/openapi.json').then(r=>r.json()).then(spec=>{
  const el=document.getElementById('ops');
  const paths=Object.keys(spec.paths).sort();
  for(const p of paths){for(const [m,op] of Object.entries(spec.paths[p])){
    const row=document.createElement('div');row.className='row';
    row.innerHTML='<span class="op '+m+'">'+m.toUpperCase()+'</span><code>'+p+'</code><span class="muted">'+(op.summary||'')+'</span>';
    el.appendChild(row);
  }}
});
</script></body></html>`
