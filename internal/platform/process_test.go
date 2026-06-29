package platform

import (
	"context"
	"testing"
)

// Verify against the live process list that Processes collects OpenFiles/Cwd only for
// processes where enrich returns true, and collects just Name/Args otherwise.
func TestProcessesEnrichGating(t *testing.T) {
	ctx := context.Background()
	all, err := Processes(ctx, func(name string, args []string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("expected at least one process")
	}

	// Predicate that matches nothing: OpenFiles/CWD must never be collected.
	none, err := Processes(ctx, func(name string, args []string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range none {
		if len(p.OpenFiles) != 0 {
			t.Fatalf("pid %d: OpenFiles should be empty when enrich=false, got %v", p.PID, p.OpenFiles)
		}
		if p.CWD != "" {
			t.Fatalf("pid %d: CWD should be empty when enrich=false, got %q", p.PID, p.CWD)
		}
	}
	// Name/Args are collected regardless of enrich.
	var named int
	for _, p := range none {
		if p.Executable != "" {
			named++
		}
	}
	if named == 0 {
		t.Fatal("expected executables to be populated regardless of enrich")
	}

	// Confirm enrich receives both Name and Args.
	sawArgs := false
	_, err = Processes(ctx, func(name string, args []string) bool {
		if len(args) > 0 {
			sawArgs = true
		}
		return false
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawArgs {
		t.Fatal("expected enrich to receive args for at least one process")
	}
}
