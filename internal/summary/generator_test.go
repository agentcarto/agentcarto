package summary

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewPicksTheConfiguredAgent(t *testing.T) {
	cases := []struct {
		agent, model string
		wantName     string
		wantErr      string
	}{
		{"claude", "claude-sonnet-5", "claude claude-sonnet-5", ""},
		{"codex", "gpt-5.5", "codex gpt-5.5", ""},
		{"", "claude-sonnet-5", "", "no agent configured"},
		{"gemini", "x", "", "unknown agent"},
		{"claude", "", "", "no model configured"},
	}
	for _, c := range cases {
		g, e := New(c.agent, c.model, time.Minute)
		switch {
		case c.wantErr == "":
			if e != nil {
				t.Errorf("New(%q,%q)=%v", c.agent, c.model, e)
				continue
			}
			if g.Name() != c.wantName {
				t.Errorf("New(%q,%q).Name()=%q want %q", c.agent, c.model, g.Name(), c.wantName)
			}
		case e == nil:
			t.Errorf("New(%q,%q) succeeded, want an error naming %q", c.agent, c.model, c.wantErr)
		case !strings.Contains(e.Error(), c.wantErr):
			t.Errorf("New(%q,%q)=%v want it to name %q", c.agent, c.model, e, c.wantErr)
		}
	}
}

// The name is stored beside every summary, so a reader can tell a Haiku summary
// from a Sonnet one without rereading the session.
func TestNameCarriesAgentAndModel(t *testing.T) {
	g, e := New("claude", "claude-haiku-4-5", time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(g.Name(), "claude-haiku-4-5") || !strings.Contains(g.Name(), "claude") {
		t.Errorf("Name()=%q", g.Name())
	}
}

func TestClaudeAnswerUnwrapsTheEnvelope(t *testing.T) {
	got, e := claudeAnswer([]byte(`{"result":"@@TURN 1\nやった","is_error":false,"total_cost_usd":0.2}`))
	if e != nil {
		t.Fatal(e)
	}
	if got != "@@TURN 1\nやった" {
		t.Errorf("answer=%q", got)
	}
}

// A run that fails still exits 0 and says so in the envelope. Treating that as
// success would store an error message as a summary.
func TestClaudeAnswerRejectsAFailedRun(t *testing.T) {
	cases := []struct{ name, in string }{
		{"is_error", `{"result":"Credit balance too low","is_error":true,"subtype":"error_during_execution"}`},
		{"empty result", `{"result":"","is_error":false}`},
		{"blank result", `{"result":"   ","is_error":false}`},
		{"not json", `Usage: claude [options]`},
	}
	for _, c := range cases {
		if _, e := claudeAnswer([]byte(c.in)); e == nil {
			t.Errorf("%s: claudeAnswer accepted %.40q", c.name, c.in)
		}
	}
}

// The error names the failing binary and shows what it said, because the only
// place these failures surface is a background worker's log.
func TestRunReportsWhatWentWrong(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the cases below drive /bin/sh")
	}
	ctx := context.Background()

	t.Run("missing binary", func(t *testing.T) {
		_, e := run(ctx, time.Minute, "", "agentcarto-no-such-binary-4f2a")
		if e == nil || !strings.Contains(e.Error(), "not on PATH") {
			t.Errorf("run=%v want it to say the binary is not on PATH", e)
		}
	})

	t.Run("non-zero exit passes stderr through", func(t *testing.T) {
		_, e := run(ctx, time.Minute, "", "sh", "-c", "echo 'not signed in' >&2; exit 3")
		if e == nil {
			t.Fatal("run succeeded on a failing command")
		}
		if !strings.Contains(e.Error(), "not signed in") {
			t.Errorf("run=%v want it to carry what the command said", e)
		}
	})

	// The timeout has to actually bound the call. Killing the process leaves Run
	// waiting on the output pipes, which a CLI that spawned a child holds open
	// for as long as the child runs — the timeout would bound nothing.
	t.Run("timeout returns without waiting for the process", func(t *testing.T) {
		start := time.Now()
		_, e := run(ctx, 50*time.Millisecond, "", "sh", "-c", "sleep 30")
		took := time.Since(start)
		if e == nil || !strings.Contains(e.Error(), "gave up after") {
			t.Errorf("run=%v want a timeout", e)
		}
		if took > 5*time.Second {
			t.Errorf("run took %s for a 50ms timeout — it waited for the process", took)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		c, cancel := context.WithCancel(ctx)
		cancel()
		_, e := run(c, time.Minute, "", "sh", "-c", "sleep 5")
		if e == nil || !strings.Contains(e.Error(), "cancelled") {
			t.Errorf("run=%v want a cancellation", e)
		}
	})
}

// A session's text is far past any platform's argument-length limit, so the
// prompt has to arrive on stdin.
func TestRunSendsThePromptOnStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("drives /bin/sh")
	}
	big := strings.Repeat("あ", 200000)
	out, e := run(context.Background(), time.Minute, big, "sh", "-c", "wc -c")
	if e != nil {
		t.Fatal(e)
	}
	// 200000 runes of 3 bytes each.
	if want := "600000"; !strings.Contains(string(out), want) {
		t.Errorf("the command saw %q on stdin, want %s bytes", strings.TrimSpace(string(out)), want)
	}
}

// End-to-end against the real CLIs, off unless asked for: it spends money and
// needs both agents installed and signed in. The shape follows
// internal/pluginhost's integration test, which skips rather than fails when
// what it drives is absent.
//
// What this covers and a mock cannot: that the option sets actually work
// together (--ignore-user-config makes -m mandatory, which only running it
// revealed), that both agents obey the same System prompt, and that what comes
// back parses. Those are the parts that break when a CLI changes under us.
func TestGenerateAgainstTheRealCLIs(t *testing.T) {
	if os.Getenv("AGENTCARTO_SUMMARY_E2E") == "" {
		t.Skip("set AGENTCARTO_SUMMARY_E2E=1 to run (spends money on the configured models)")
	}
	doc := "# t\n\n- **Turns**: 2\n\n## Turn 1 — 2026-08-21 03:01:58\n\n**USER**\n\n" +
		"詳細ビューでコピーするキーを追加したい\n\n- Bash $ grep -rn \"clipboard\" internal/\n\n" +
		"**ASSISTANT**\n\nOSC 52 を使う termenv.Copy で clipboard.go を新規作成し、y キーを配線した。\n\n" +
		"## Turn 2 — 2026-08-21 03:17:33\n\n**USER**\n\n一回コミット\n\n" +
		"- Bash $ git commit -m \"feat(tui): copy a turn with y\"\n\n**ASSISTANT**\n\n3a2b100 でコミットした。\n"

	for _, tc := range []struct{ agent, model string }{
		{"claude", "claude-sonnet-5"},
		{"codex", "gpt-5.5"},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			g, e := New(tc.agent, tc.model, 5*time.Minute)
			if e != nil {
				t.Fatal(e)
			}
			out, e := g.Generate(context.Background(), System(""), doc)
			if e != nil {
				t.Fatalf("Generate: %v", e)
			}
			r, e := Parse(out, []int{1, 2})
			if e != nil {
				t.Fatalf("Parse: %v\n--- what came back ---\n%s", e, out)
			}
			if m := r.Missing([]int{1, 2}); len(m) > 0 {
				t.Errorf("turns %v came back with no summary", m)
			}
			// The instruction says to keep identifiers as they are. A summary
			// that drops the commit hash is not one a reader can act on.
			if !strings.Contains(r.Turns[2], "3a2b100") {
				t.Errorf("turn 2 dropped the commit hash: %q", r.Turns[2])
			}
			if r.Session == "" {
				t.Error("no session summary came back")
			}
		})
	}
}
