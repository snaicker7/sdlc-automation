// Package agent wraps the LLM workers. The orchestrator only sees Runner, so the
// backend (Claude Code headless, Agent SDK sidecar, mock) is swappable.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// Tier selects a model class (model routing: strong for design/review, fast for formatting).
type Tier string

const (
	TierStrong Tier = "strong"
	TierCode   Tier = "code"
	TierFast   Tier = "fast"
)

type Request struct {
	Role   string // e.g. prd-writer, critic, implementer; also the mock's dispatch key
	Tier   Tier
	System string   // role instructions
	Prompt string   // task + context
	Dir    string   // working dir; empty = scratch dir with no tools
	Tools  []string // allowed tools; empty = text-only
}

type Result struct {
	Text       string
	Model      string
	CostUSD    float64
	InTokens   int
	OutTokens  int
	Transcript string
}

type Runner interface {
	Run(ctx context.Context, req Request) (Result, error)
}

// ExtractJSON pulls the first JSON object/array out of an LLM reply (handles code fences/prose).
func ExtractJSON(text string, v any) error {
	t := strings.TrimSpace(text)
	start := strings.IndexAny(t, "{[")
	if start < 0 {
		return errors.New("no JSON found in agent output")
	}
	closer := byte('}')
	if t[start] == '[' {
		closer = ']'
	}
	end := strings.LastIndexByte(t, closer)
	if end < start {
		return errors.New("unterminated JSON in agent output")
	}
	return json.Unmarshal([]byte(t[start:end+1]), v)
}
