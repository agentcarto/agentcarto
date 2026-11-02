//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agentcarto/agentcarto/internal/config"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/core/domain"
)

// Kill is how `doctor` says to end a run. The call it was making has to stop with it,
// nothing more may be called, and nothing may be left that reads as a worker running.
func TestASignalStopsTheWorkerAndItsCall(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "signalled", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1", 2: "n2"}, Batches: [][]int{{1}, {2}}, Prompts: []string{"a", "b"},
	}
	d, q := stoppedFixture(t, r)
	next := summary.Request{
		PluginID: "claude", SessionID: "next", Queued: time.Now(), Fingerprint: "fp2",
		Nodes: map[int]string{1: "n1"}, Batches: [][]int{{1}}, Prompts: []string{"c"},
	}
	if err := q.Add(next); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	pidFile, callsFile := filepath.Join(bin, "pid"), filepath.Join(bin, "calls")
	stub := "#!/bin/sh\necho call >> " + callsFile + "\necho $$ > " + pidFile + ".tmp\nmv " + pidFile + ".tmp " + pidFile + "\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := config.Config{Summary: config.Summary{Agent: "claude", Model: "m", MaxPerRun: 2}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		summarizeWorkerCmd(context.Background(), cfg, d, nil)
	}()

	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker never made its call")
		}
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not stop")
	}

	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the call outlived the worker: signal 0 to %d gave %v", pid, err)
	}
	if _, err := os.Stat(lockPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the lock outlived the worker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(queueDir(), ".working")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the record of its call outlived the worker: %v", err)
	}
	calls, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(calls), "call"); n != 1 {
		t.Errorf("the worker made %d calls, the last of them after it was stopped", n)
	}
	for _, id := range []string{"signalled", "next"} {
		back, still := q.Find("claude", id)
		if !still {
			t.Errorf("%s was dropped, so nothing will ask for this summary again", id)
		} else if back.Attempts != 0 {
			t.Errorf("being stopped was recorded as a failure of %s: attempts=%d reason=%q", id, back.Attempts, back.LastError)
		}
	}
	log, err := os.ReadFile(logPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "stopped by a signal") {
		t.Errorf("the log does not say what happened:\n%s", log)
	}
}

// show making a session's one call is a foreground version of the same thing. Stopped,
// it stops the call, keeps the request for later, and says so to the command.
func TestASignalStopsTheCallShowIsMaking(t *testing.T) {
	r := summary.Request{
		PluginID: "claude", SessionID: "shown", Queued: time.Now(), Fingerprint: "fp1",
		Nodes: map[int]string{1: "n1"}, Batches: [][]int{{1}}, Prompts: []string{"a"},
	}
	d, q := stoppedFixture(t, r)
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "pid")
	stub := "#!/bin/sh\necho $$ > " + pidFile + ".tmp\nmv " + pidFile + ".tmp " + pidFile + "\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := config.Config{Summary: config.Summary{Agent: "claude", Model: "m"}}
	s := domain.Session{PluginID: "claude", SessionID: "shown", Fingerprint: "fp1"}

	type result struct{ ready, stopped bool }
	done := make(chan result, 1)
	go func() {
		ready, stopped := summarizeForShow(context.Background(), nil, cfg, d, s, "shown")
		done <- result{ready, stopped}
	}()

	var pid int
	for deadline := time.Now().Add(10 * time.Second); pid == 0; time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if time.Now().After(deadline) {
			t.Fatal("show never made its call")
		}
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("show did not stop")
	}

	if !got.stopped || got.ready {
		t.Errorf("ready=%v stopped=%v, want a stop", got.ready, got.stopped)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the call outlived show: signal 0 to %d gave %v", pid, err)
	}
	back, still := q.Find("claude", "shown")
	if !still {
		t.Fatal("the request was lost")
	}
	if back.Attempts != 0 || back.LastError != "" {
		t.Errorf("being stopped was recorded as a failure: attempts=%d reason=%q", back.Attempts, back.LastError)
	}
}
