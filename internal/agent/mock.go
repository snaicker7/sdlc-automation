package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Mock is a deterministic stand-in for Claude, used to exercise the orchestrator
// (state machine, ledger, gates, loops) fast and for free. It deliberately triggers
// the critique loop (first PRD draft lacks NFRs) and the review-fix loop.
type Mock struct{}

var (
	reqRe    = regexp.MustCompile(`REQ-\d+`)
	ticketRe = regexp.MustCompile(`TICKET:\s*([A-Z]+-\d+)`)
)

func (Mock) Run(_ context.Context, r Request) (Result, error) {
	res := Result{Model: "mock", InTokens: 100, OutTokens: 100}
	revising := strings.Contains(r.Prompt, "REVISION FEEDBACK")
	switch r.Role {
	case "clarifier-prd":
		res.Text = `[{"text":"Which document formats must be supported in v1?","priority":1,"proposed_default":"Plain text and Markdown only"},
{"text":"Is a single-user local app sufficient, or do we need accounts?","priority":1,"proposed_default":"Single user, no auth"}]`
	case "clarifier-add":
		res.Text = `[{"text":"Where should documents be persisted?","priority":1,"proposed_default":"JSON file on local disk"}]`
	case "prd-writer":
		nfr := ""
		if revising {
			nfr = "\n## Non-functional requirements\n- Responses under 200ms for 10k documents.\n"
		}
		res.Text = `# PRD: Document Organizer
## Problem
Users cannot find their notes.
## Requirements
- REQ-1: Users can add a document with title, body and tags. Acceptance: POST creates it; it is retrievable.
- REQ-2: Users can search documents by tag or text. Acceptance: search returns only matching documents.
## Assumptions
(see ledger)
` + nfr
	case "add-writer":
		res.Text = `# ADD: Document Organizer
## Components
Single Go service, JSON file store.
## REQ-1 Add documents
Store interface + file-backed implementation.
## REQ-2 Search
In-memory index rebuilt from the store.
## Failure modes
Corrupt file: refuse to start, keep backup.
## Rollout
Local only.
`
	case "breakdown-writer":
		res.Text = `{"repos":[{"name":"doc-organizer","summary":"Go service with CLI/HTTP","contracts_first":false,"depends_on":[]}]}`
	case "jira-writer":
		res.Text = `{"epic":{"summary":"Document Organizer v1"},"stories":[
{"summary":"Add and retrieve documents","repo":"doc-organizer","estimate":3,"reqs":["REQ-1"],"acceptance":["A document can be added","A document can be fetched by id"],"description":"Implement the store and add/get."},
{"summary":"Search documents","repo":"doc-organizer","estimate":3,"reqs":["REQ-2"],"acceptance":["Search by tag works","Search by text works"],"description":"Implement search over the store."}]}`
	case "critic":
		blocking := "[]"
		if strings.Contains(r.Prompt, "KIND: prd.md") && !strings.Contains(r.Prompt, "Non-functional") {
			blocking = `["PRD has no non-functional requirements (latency, scale)."]`
		}
		res.Text = `{"blocking":` + blocking + `,"notes":["mock critic reviewed the artifact"]}`
	case "planner":
		res.Text = "## Plan\n1. Write a failing test.\n2. Implement.\n3. Run `go test ./...`.\n"
	case "implementer", "fixer":
		m := ticketRe.FindStringSubmatch(r.Prompt)
		if m == nil || r.Dir == "" {
			return res, fmt.Errorf("mock %s: need TICKET and Dir", r.Role)
		}
		name := strings.ReplaceAll(m[1], "-", "")
		if r.Role == "fixer" {
			if err := os.WriteFile(filepath.Join(r.Dir, "REVIEW_FIXED.md"), []byte("context propagation added\n"), 0o644); err != nil {
				return res, err
			}
			res.Text = "fixed review findings"
			break
		}
		src := fmt.Sprintf("package main\n\nfunc Feature%s() string { return %q }\n", name, m[1])
		tst := fmt.Sprintf("package main\n\nimport \"testing\"\n\nfunc TestFeature%s(t *testing.T) {\n\tif Feature%s() != %q {\n\t\tt.Fatal(\"unexpected\")\n\t}\n}\n", name, name, m[1])
		if err := os.WriteFile(filepath.Join(r.Dir, "feature_"+name+".go"), []byte(src), 0o644); err != nil {
			return res, err
		}
		if err := os.WriteFile(filepath.Join(r.Dir, "feature_"+name+"_test.go"), []byte(tst), 0o644); err != nil {
			return res, err
		}
		res.Text = "implemented " + m[1]
	case "mr-writer":
		res.Text = "## Summary\nImplements the ticket per the approved plan.\n## Risk\nLow.\n"
	case "reviewer":
		if _, err := os.Stat(filepath.Join(r.Dir, "REVIEW_FIXED.md")); err != nil {
			res.Text = `{"findings":[{"severity":"blocking","file":"main.go","summary":"Context is not propagated to the store call.","suggestion":"Thread ctx through."},{"severity":"info","summary":"Consider table-driven tests."}]}`
		} else {
			res.Text = `{"findings":[{"severity":"info","summary":"Consider table-driven tests."}]}`
		}
	default:
		return res, fmt.Errorf("mock: unknown role %q", r.Role)
	}
	return res, nil
}
