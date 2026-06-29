package catalog

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
)

type Catalog struct{ Plugins []plugin.Instance }

func (c Catalog) Scan(ctx context.Context, warm []domain.Session, deadWarm map[string]string) domain.Snapshot {
	// The previous results (warm/dead) are handed to each plugin in bulk; the
	// incremental scan (fingerprinting, reuse/skip decisions, and
	// ParserVersion/Fingerprint backfill) happens on the plugin side (core/scan).
	// warm is split per plugin to reduce IPC volume; dead is passed whole because
	// a path is unique across plugins, and each plugin carries forward only the
	// entries for its own paths.
	warmByPlugin := groupWarmByPlugin(warm)
	snap := domain.Snapshot{Dead: map[string]string{}}
	for r := range c.scanAll(ctx, warmByPlugin, deadWarm) {
		snap.Sessions = append(snap.Sessions, r.sessions...)
		for k, v := range r.dead {
			snap.Dead[k] = v
		}
		if r.err.Err != nil {
			snap.Errors = append(snap.Errors, r.err)
		}
	}
	backfillCopilotUnknownCWD(snap.Sessions, 6*time.Hour)
	sort.SliceStable(snap.Sessions, func(i, j int) bool { return snap.Sessions[i].UpdatedAt.After(snap.Sessions[j].UpdatedAt) })
	snap.Version = 1
	return snap
}

// groupWarmByPlugin buckets the warm sessions by plugin ID, dropping any without
// a source ref (which cannot be matched back during a scan).
func groupWarmByPlugin(warm []domain.Session) map[string][]domain.Session {
	byPlugin := make(map[string][]domain.Session)
	for _, s := range warm {
		if s.SourceRef.Source == "" {
			continue
		}
		byPlugin[s.PluginID] = append(byPlugin[s.PluginID], s)
	}
	return byPlugin
}

type scanResult struct {
	sessions []domain.Session
	dead     map[string]string
	err      domain.PluginError
}

// scanAll runs every Scanner plugin concurrently and returns a channel that
// delivers one result per plugin; it is closed once all plugins have finished.
func (c Catalog) scanAll(ctx context.Context, warmByPlugin map[string][]domain.Session, deadWarm map[string]string) <-chan scanResult {
	ch := make(chan scanResult, len(c.Plugins))
	var wg sync.WaitGroup
	for _, p := range c.Plugins {
		sc, ok := p.Impl.(plugin.Scanner)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(p plugin.Instance, sc plugin.Scanner) {
			defer wg.Done()
			defer func() {
				if v := recover(); v != nil {
					ch <- scanResult{err: domain.PluginError{PluginID: p.ID, Reason: "plugin panic", Err: fmt.Errorf("%v", v)}}
				}
			}()
			out, e := sc.Scan(ctx, plugin.ScanInput{Warm: warmByPlugin[p.ID], Dead: deadWarm})
			if e != nil {
				ch <- scanResult{err: domain.PluginError{PluginID: p.ID, Reason: "scan failed", Err: e}}
				return
			}
			ch <- scanResult{sessions: out.Sessions, dead: out.Dead}
		}(p, sc)
	}
	go func() { wg.Wait(); close(ch) }()
	return ch
}

func backfillCopilotUnknownCWD(sessions []domain.Session, maxGap time.Duration) {
	for i := range sessions {
		// Only the copilot family (VS Code / JetBrains) is eligible. AgentType is
		// driven by the plugin and is robust against ID renames.
		if !isCopilot(sessions[i].AgentType) || sessions[i].CWD != "(unknown)" {
			continue
		}
		if cwd := nearestKnownCWD(sessions, i, maxGap); cwd != "" {
			sessions[i].CWD = cwd
		}
	}
}

func isCopilot(agentType string) bool {
	return agentType == "copilot-vc" || agentType == "copilot-jb"
}

// nearestKnownCWD returns the CWD of the session closest in time to sessions[i]
// (within maxGap) that has a known working directory, or "" if none qualifies.
func nearestKnownCWD(sessions []domain.Session, i int, maxGap time.Duration) string {
	t := sessionTime(sessions[i])
	if t.IsZero() {
		return ""
	}
	bestCWD := ""
	bestGap := time.Duration(0)
	for j := range sessions {
		if i == j || sessions[j].CWD == "" || sessions[j].CWD == "(unknown)" {
			continue
		}
		ot := sessionTime(sessions[j])
		if ot.IsZero() {
			continue
		}
		gap := absDuration(t.Sub(ot))
		if gap > maxGap {
			continue
		}
		if bestCWD == "" || gap < bestGap {
			bestCWD = sessions[j].CWD
			bestGap = gap
		}
	}
	return bestCWD
}

func sessionTime(s domain.Session) time.Time {
	if !s.StartedAt.IsZero() {
		return s.StartedAt
	}
	return s.UpdatedAt
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (c Catalog) Plugin(id string) (plugin.Instance, bool) {
	for _, p := range c.Plugins {
		if p.ID == id {
			return p, true
		}
	}
	return plugin.Instance{}, false
}
