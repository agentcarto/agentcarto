package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestShellCommandEmptyDir(t *testing.T) {
	if _, e := ShellCommand(""); e == nil {
		t.Fatal("empty dir should be rejected")
	}
}

func TestShellCommandMissingDir(t *testing.T) {
	if _, e := ShellCommand(filepath.Join(t.TempDir(), "gone")); e == nil {
		t.Fatal("missing dir should be rejected")
	}
}

func TestShellCommandFileIsNotDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "f")
	if e := os.WriteFile(f, nil, 0o644); e != nil {
		t.Fatal(e)
	}
	if _, e := ShellCommand(f); e == nil {
		t.Fatal("regular file should be rejected")
	}
}

func TestShellCommandUsesShellEnv(t *testing.T) {
	env := "SHELL"
	if runtime.GOOS == "windows" {
		env = "COMSPEC"
	}
	t.Setenv(env, "/opt/fancy/sh")
	dir := t.TempDir()
	c, e := ShellCommand(dir)
	if e != nil {
		t.Fatal(e)
	}
	if c.Executable != "/opt/fancy/sh" || c.WorkingDirectory != dir || len(c.Args) != 0 {
		t.Fatalf("unexpected command: %+v", c)
	}
}

func TestShellCommandFallback(t *testing.T) {
	env, want := "SHELL", "/bin/sh"
	if runtime.GOOS == "windows" {
		env, want = "COMSPEC", "cmd"
	}
	t.Setenv(env, "")
	c, e := ShellCommand(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if c.Executable != want {
		t.Fatalf("expected fallback %q, got %q", want, c.Executable)
	}
}
