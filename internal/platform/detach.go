package platform

import (
	"fmt"
	"os"
	"os/exec"
)

// Detach starts this program again with the given arguments and does not wait
// for it. The child outlives the caller: it keeps running when the TUI is quit
// or the CLI command that started it returns.
//
// That is the point. Summarizing a session runs for minutes and costs money per
// call, so tying it to a foreground process means a person closing the TUI
// throws away work that was already paid for. Detaching moves the lifetime of
// the work off the lifetime of whoever asked for it.
//
// The child gets no standard streams. Writing to a terminal the parent has
// restored would corrupt whatever is on screen afterwards, and a pipe left open
// would keep the parent's shell waiting for a process it does not know about.
func Detach(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot find this program to start it again: %w", err)
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Release lets the parent exit without leaving the child a zombie. Nothing
	// here waits for it — what it did is in its log and in the cache.
	return cmd.Process.Release()
}
