package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Claude drives Claude Code headless (`claude -p`). The prompt goes over stdin to
// avoid shell-quoting problems; results come back as JSON with cost + usage.
type Claude struct {
	Bin        string
	Models     map[Tier]string
	Timeout    time.Duration
	ScratchDir string
}

func NewClaude(models map[Tier]string) *Claude {
	return &Claude{Bin: "claude", Models: models, Timeout: 20 * time.Minute}
}

type claudeJSON struct {
	Result       string  `json:"result"`
	IsError      bool    `json:"is_error"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        struct {
		In  int `json:"input_tokens"`
		Out int `json:"output_tokens"`
	} `json:"usage"`
}

func (c *Claude) Run(ctx context.Context, req Request) (Result, error) {
	model := c.Models[req.Tier]
	args := []string{"-p", "--output-format", "json", "--no-session-persistence"}
	if model != "" {
		args = append(args, "--model", model)
	}
	if req.System != "" {
		args = append(args, "--append-system-prompt", req.System)
	}
	if len(req.Tools) > 0 {
		args = append(args, "--allowedTools", strings.Join(req.Tools, ","), "--permission-mode", "acceptEdits")
	} else {
		args = append(args, "--tools", "")
	}

	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	dir := req.Dir
	if dir == "" {
		dir = c.ScratchDir
		if dir == "" {
			dir = os.TempDir()
		}
	}
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(req.Prompt)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return Result{}, fmt.Errorf("claude %s: %w: %s", req.Role, err, truncate(errb.String()+out.String(), 800))
	}
	var j claudeJSON
	if err := json.Unmarshal(out.Bytes(), &j); err != nil {
		return Result{}, fmt.Errorf("claude %s: bad json output: %w: %s", req.Role, err, truncate(out.String(), 400))
	}
	if j.IsError {
		return Result{}, fmt.Errorf("claude %s reported error: %s", req.Role, truncate(j.Result, 400))
	}
	return Result{Text: j.Result, Model: model, CostUSD: j.TotalCostUSD,
		InTokens: j.Usage.In, OutTokens: j.Usage.Out, Transcript: out.String()}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
