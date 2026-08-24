package summary

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Generator runs an agent over a prompt and returns what it said. The interface
// exists so that the part of this package that decides what a summary says can
// be tested without a subprocess, and so that a future direct API call can
// replace the CLI without touching anything else.
type Generator interface {
	Generate(ctx context.Context, system, prompt string) (string, error)
	// Name is what gets stored beside the summary ("claude claude-sonnet-5"),
	// so a reader can tell which agent and model wrote it.
	Name() string
}

// New returns the generator for an agent name, as configured. An empty agent is
// the off switch and is an error here: the callers that respect the off switch
// check it before asking for a generator, so reaching this with an empty name
// means something asked for a summary that configuration says not to make.
func New(agent, model string, timeout time.Duration) (Generator, error) {
	// The agent comes first. With both fields empty — which configuration
	// allows, since an empty agent switches the feature off and the rest is then
	// left unchecked — saying "no model configured" would send the reader after
	// the wrong setting.
	switch agent {
	case "claude", "codex":
	case "":
		return nil, errors.New("summary: no agent configured (set summary.agent to claude or codex)")
	default:
		return nil, fmt.Errorf("summary: unknown agent %q", agent)
	}
	if model == "" {
		return nil, fmt.Errorf("summary: no model configured for agent %q", agent)
	}
	if agent == "codex" {
		return codexCLI{model: model, timeout: timeout}, nil
	}
	return claudeCLI{model: model, timeout: timeout}, nil
}

// claudeCLI drives `claude -p`.
type claudeCLI struct {
	model   string
	timeout time.Duration
}

func (c claudeCLI) Name() string { return "claude " + c.model }

func (c claudeCLI) Generate(ctx context.Context, system, prompt string) (string, error) {
	// --no-session-persistence matters more than it looks: without it every
	// summary run leaves a session log of its own on disk, which agentcarto then
	// lists and offers to summarize. Summarizing would breed summarizable
	// sessions.
	//
	// --setting-sources '' keeps the user's CLAUDE.md and settings out of a run
	// whose whole input is supposed to be the prompt, and --allowed-tools ''
	// leaves it nothing to do but answer.
	out, e := run(ctx, c.timeout, prompt, "claude",
		"-p",
		"--model", c.model,
		"--system-prompt", system,
		"--no-session-persistence",
		"--setting-sources", "",
		"--allowed-tools", "",
		"--output-format", "json",
	)
	if e != nil {
		return "", e
	}
	return claudeAnswer(out)
}

// claudeAnswer unwraps `--output-format json`. The envelope carries the answer
// plus what the call cost, and is_error marks a run that finished but failed —
// which exits 0, so the exit status alone would call it a success.
func claudeAnswer(out []byte) (string, error) {
	var env struct {
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		Subtype string `json:"subtype"`
	}
	if e := json.Unmarshal(out, &env); e != nil {
		return "", fmt.Errorf("claude: unreadable output: %w (began %.120q)", e, out)
	}
	if env.IsError {
		return "", fmt.Errorf("claude: the run failed (%s): %.200s", env.Subtype, env.Result)
	}
	if strings.TrimSpace(env.Result) == "" {
		return "", fmt.Errorf("claude: the run returned an empty answer")
	}
	return env.Result, nil
}

// codexCLI drives `codex exec`.
type codexCLI struct {
	model   string
	timeout time.Duration
}

func (c codexCLI) Name() string { return "codex " + c.model }

func (c codexCLI) Generate(ctx context.Context, system, prompt string) (string, error) {
	// codex has no system-prompt option, so the instruction goes at the top of
	// the prompt itself.
	//
	// --ephemeral is the counterpart of claude's --no-session-persistence.
	// --ignore-user-config keeps the user's config.toml out (which is why -m is
	// not optional here), -s read-only stops it writing anything, and
	// --skip-git-repo-check lets it run outside a repository. There is no option
	// that switches tools off entirely; read-only plus the instruction in the
	// prompt is as far as this goes.
	//
	// The answer is asked for as a file rather than read from stdout, which
	// carries progress lines as well.
	dir, e := os.MkdirTemp("", "agentcarto-summary-")
	if e != nil {
		return "", fmt.Errorf("codex: %w", e)
	}
	defer os.RemoveAll(dir)
	last := filepath.Join(dir, "last-message.txt")

	if _, e := run(ctx, c.timeout, system+"\n\n---\n\n"+prompt, "codex", "exec",
		"-m", c.model,
		"--ephemeral",
		"--ignore-user-config",
		"--skip-git-repo-check",
		"-s", "read-only",
		"-o", last,
	); e != nil {
		return "", e
	}
	b, e := os.ReadFile(last)
	if e != nil {
		return "", fmt.Errorf("codex: the run wrote no answer: %w", e)
	}
	// An empty answer is a failure here rather than further down in Parse, so
	// that a log line about a codex run reads as a codex problem.
	if strings.TrimSpace(string(b)) == "" {
		return "", errors.New("codex: the run returned an empty answer")
	}
	return string(b), nil
}

// run executes one agent CLI with the prompt on stdin and returns its stdout.
//
// The prompt goes through stdin rather than an argument because a session's
// text is far past any platform's argument-length limit.
func run(ctx context.Context, timeout time.Duration, stdin, name string, args ...string) ([]byte, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	// Killing the process on timeout is not enough to get Run to return: it
	// waits for the output pipes to close, and a CLI that spawned a child leaves
	// them open for as long as the child lives. Without this, a timeout that was
	// supposed to bound the run does not bound it at all — measured at five
	// seconds for a 50ms timeout over `sh -c 'sleep 5'`. WaitDelay gives the
	// pipes a moment to drain and then stops waiting.
	cmd.WaitDelay = 2 * time.Second
	switch e := cmd.Run(); {
	case e == nil:
		return out.Bytes(), nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return nil, fmt.Errorf("%s: gave up after %s", name, timeout)
	case errors.Is(ctx.Err(), context.Canceled):
		return nil, fmt.Errorf("%s: cancelled", name)
	case errors.Is(e, exec.ErrNotFound):
		return nil, fmt.Errorf("%s: not on PATH — summaries need its CLI installed and signed in", name)
	default:
		// The CLIs report an unusable state (not signed in, unknown model) on
		// stderr and exit non-zero. Passing it through is what makes the failure
		// diagnosable from a background worker's log.
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return nil, fmt.Errorf("%s: %w: %.400s", name, e, msg)
	}
}
