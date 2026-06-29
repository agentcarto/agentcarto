package platform

import (
	"context"
	"github.com/agentcarto/core/domain"
	"github.com/shirou/gopsutil/v4/process"
)

// Processes returns the list of processes on the system. Name/Args are collected for every
// process, but the more expensive Cwd and OpenFiles lookups (on Linux the latter readlinks
// each fd) are limited to processes for which enrich(name, args) returns true (i.e. agent
// candidates). When enrich is nil, those fields are collected for every process.
func Processes(ctx context.Context, enrich func(name string, args []string) bool) ([]domain.Process, error) {
	ps, e := process.ProcessesWithContext(ctx)
	if e != nil {
		return nil, e
	}
	out := make([]domain.Process, 0, len(ps))
	for _, p := range ps {
		name, _ := p.NameWithContext(ctx)
		args, _ := p.CmdlineSliceWithContext(ctx)
		x := domain.Process{PID: p.Pid, Executable: name, Args: args}
		if enrich == nil || enrich(name, args) {
			x.CWD, _ = p.CwdWithContext(ctx)
			fs, _ := p.OpenFilesWithContext(ctx)
			for _, f := range fs {
				x.OpenFiles = append(x.OpenFiles, f.Path)
			}
		}
		out = append(out, x)
	}
	return out, nil
}
