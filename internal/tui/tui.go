package tui

import (
	"context"
	"fmt"
	"github.com/agentcarto/agentcarto/internal/app"
	"github.com/agentcarto/agentcarto/internal/cache"
	searchpkg "github.com/agentcarto/agentcarto/internal/search"
	"github.com/agentcarto/agentcarto/internal/summary"
	"github.com/agentcarto/agentcarto/internal/transcript"
	convlogic "github.com/agentcarto/core/conversation"
	"github.com/agentcarto/core/domain"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/muesli/termenv"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type scanMsg struct {
	snap domain.Snapshot
	// sessions is what the list shows: the scan's own sessions plus the ones the
	// cache remembers and the scan could not find. snap.Sessions stays as it came
	// back, because that is what the cache is told it has seen — recording a
	// deleted session as seen just now would make it look alive to everything
	// that asks when a session was last there.
	sessions []domain.Session
	parents  map[domain.SessionKey]string
	index    *searchpkg.Index
	indexFP  map[string]string
}
type convMsg struct {
	c         *domain.Conversation
	focusLeaf string // ID in the synthesized tree matching the opened fork's active leaf (used to focus that branch in the root-anchored view)
	origins   map[string]app.NodeOrigin
	key       domain.SessionKey // session the load was requested for (stale results are dropped)
	e         error
	reset     bool
}
type tickMsg time.Time
type blinkMsg time.Time

// flashExpireMsg is the expiry timer for a flash message (armed each time a flash
// is set, and delivered flashTTL later).
type flashExpireMsg struct{}

// blinkInterval is the length of one phase of the blink that animates the elapsed
// time of an in-progress turn (on/off makes one full cycle).
const blinkInterval = 500 * time.Millisecond

// flashTTL is how long a transient footer message (flash) stays visible before it
// is automatically cleared.
const flashTTL = 4 * time.Second

// maxSessionTreeDepth limits only the visual indentation in the session list.
// The forest keeps the full logical depth so ordering and session actions are
// unaffected by long chains of forks.
const maxSessionTreeDepth = 2

type Model struct {
	// afterScan runs once each scan finishes, with the sessions it found. It is
	// how summarizing gets started without the TUI knowing what summarizing is:
	// generating costs money and talks to another program, neither of which
	// belongs in a view. Nil when the host has nothing to do there.
	afterScan func([]domain.Session)
	// summaries are the generated descriptions of the session on screen, keyed by
	// the turn number the detail view shows; key 0 is the session's own summary,
	// the paragraph the head row carries. Stored whole rather than as text alone,
	// because which model wrote one is per summary: the session's and a turn's can
	// come from different runs. Read once with the conversation rather than per
	// turn, since opening a turn should not wait on a query.
	summaries map[int]cache.Summary
	// summaryAt is when the session's own summary was written. Zero when there is
	// none or the row predates the field being stored.
	summaryAt time.Time
	// summaryStale says the session went on after its own summary was written,
	// which only the session summary can be: a turn's is withheld unless it
	// belongs to the turn shown.
	summaryStale bool
	// headlines are the one-line summaries of every session that has one, keyed by
	// session. The list draws one line per session, so they are read in one query
	// and refreshed with each scan (the worker writes new ones as it goes). Whole
	// rows, because the list has to say when one describes a session that has
	// since gone on.
	headlines map[domain.SessionKey]cache.Summary
	// listTitles puts the session list's last column back to the title — the
	// session's first prompt. The zero value is the headline: what a session was
	// about is what the list is read for, and the prompt that started it is one
	// Enter away. Sessions with no headline yet show their title either way.
	listTitles bool
	// summaryOpen expands the detail header's head line into the title in full
	// and the whole summary under it. The zero value is the one-line form, which
	// is what the header has always been. Kept across sessions within one run, so
	// opening it once keeps it open.
	summaryOpen       bool
	app               *app.App
	sessions          []domain.Session
	sessionParents    map[domain.SessionKey]string // canonical conversation parent; absent entries fall back to ParentSessionID
	dead              map[string]string
	indexFP           map[string]string
	filtered          []int
	rows              []listRow       // rows shown / targeted by the cursor (in folder view: headers plus the sessions of expanded groups)
	collapsed         map[string]bool // cwds that are collapsed (folder-view group open/close). nil/absent means expanded
	cursor            int
	offset            int
	width, height     int
	query             textinput.Model
	searching         bool
	detail            *domain.Conversation
	detailSession     *domain.Session
	detailOrigins     map[string]app.NodeOrigin // synthesized-tree node ID -> owning session + real node ID
	detailTurns       [][]string
	detailRows        []detailRow
	detailPathStack   []detailFrame
	detailActive      map[string]bool // nodes on the displayed detail path, rebuilt with detailRows
	detailNewestChron int             // chronological index of the newest displayed turn (for the in-progress ●). -1 if none.
	blinkOn           bool            // blink phase for the elapsed time of the selected in-progress turn (true=background lit, false=no background).
	detailCursor      int
	detailOffset      int
	turnOpen          bool
	turnBlocks        []turnBlock
	turnExpanded      map[int]bool
	turnCursor        int
	turnOffset        int
	turnSearching     bool   // entering a search query in the turn list
	turnQuery         string // search query for the turn list
	turnSearchPos     int    // current position within the hit list (-1 = none selected)
	// turnHits are the detailRows indices matching turnQuery. Finding them reads
	// the text of every turn, i.e. the whole conversation, so it is derived state
	// kept up to date by syncTurnHits on each update; a render only reads it.
	turnHits          []int
	turnHitsQuery     string // the query turnHits was computed for
	turnHitsStale     bool   // set when detailRows is rebuilt, invalidating the indices
	turnFullSearching bool   // entering a search query in the full turn view
	turnFullQuery     string // search query for the full turn view
	flash             string
	flashAt           time.Time // time the flash was last set; cleared automatically once flashTTL elapses.
	scanning          bool
	scanned           bool // whether the first scan has completed. The single startup scan reparses fully instead of using the cache.
	view              string
	activeOnly        bool
	cache             *cache.DB
	index             *searchpkg.Index
	action            string
	relocInput        textinput.Model // path input field for relocate (independent of the search query)
	relocOld          string          // source cwd of the relocate (the target chosen with the m key)
	relocCount        int             // number of sessions being relocated (for the confirm prompt)
	relocCycle        *relocCycle     // Tab-completion cycling state (input phase)
	relocCands        []string        // candidate list currently shown for Tab completion (for the zsh-style grid)
	pendingPlan       domain.MutationPlan
	pendingCmd        domain.Command
	forkBack          []domain.Session
	// launch is the command to hand off after the TUI exits (resume / fork).
	// A child process cannot be started while we still hold the alt-screen, so we
	// stash it here, tea.Quit, and let the caller exec it after the terminal is restored.
	launch *domain.Command
}

// listRow is one row of the list view. In folder view, cwd header rows and the
// session rows of expanded groups are interleaved ("header"/"session").
// Session rows of collapsed groups are not included in rows, so every row is a
// valid cursor target.
type listRow struct {
	Kind       string // "header" | "session"
	CWD        string
	SessIdx    int // index into m.sessions, for "session" rows
	Collapsed  bool
	Depth      int    // fork nesting depth (0 = root)
	TreePrefix string // leading tree connector or bounded lineage marker. Empty at depth 0.
}

type turnBlock struct {
	Sym, Style, Label string
	// LabelSpans, when set, renders the collapsed/expanded header line in
	// per-segment colors. Label must equal the concatenation of the span
	// texts (search and cursor rendering use the plain Label text).
	LabelSpans []labelSpan
	Body       []string
	Open       bool
	Time       time.Time // event timestamp shown as an HH:MM:SS gutter (zero = blank)
	// NoGutter renders the block flush left, without the timestamp gutter.
	// Used for the synthetic edited-files section, which carries no event time.
	NoGutter bool
	// Path is the absolute path of the file an edited-files block stands for, so
	// the block can be opened in an editor. The label holds only a shortened,
	// cwd-relative rendering of it. Empty on every other kind of block.
	Path string
}

// labelSpan is one segment of a header line with its own style.
type labelSpan struct {
	text, style string
}

type detailRow struct {
	Kind       string
	Turn       []string // displayed turn, or the owning turn whose headline column a branch row aligns with
	Root       string
	TurnIndex  int // chronological index of the turn row (0-based; +1 for the # label). May be non-contiguous because compact turns are skipped.
	LastBranch bool
	Badge      bool // /compact boundary badge (leading »)
	Line       int  // which of detailHeadLines this row draws ("head" is 0, "headtext" the rest)
}

// detailFrame is one navigation level of the current branch (its path plus a
// breadcrumb label). The base frame is "current"; frames pushed when descending
// into another lineage are labeled per branch.
type detailFrame struct {
	path        []string
	label       string
	followFocus bool // true only for the fork frame created by directly opening a fork session
}

func New(a *app.App, cached []domain.Session, db *cache.DB) Model {
	q := textinput.New()
	q.Prompt = "/ "
	ri := textinput.New()
	ri.Prompt = ""
	// The cache is also where a session whose log is gone is read from, so the app
	// is given it here rather than only using it for the index.
	if db != nil {
		a.Store = db
	}
	m := Model{app: a, sessions: cached, query: q, relocInput: ri, scanning: true, view: a.Config.UI.DefaultView, cache: db}
	if db != nil {
		m.headlines = db.Headlines(context.Background())
	}
	m.filter()
	return m
}
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.scan(), tick(time.Duration(m.app.Config.UI.RescanInterval)), blinkTick())
}
func (m Model) scan() tea.Cmd {
	// The first scan after startup reparses everything instead of using the warm
	// cache. This guarantees that each launch follows the latest parse results even
	// if a parser change shipped without bumping ParserVersion (render cached rows
	// immediately, refetch fully in the background, then swap). Later periodic
	// rescans are incremental and cheap.
	full := !m.scanned
	return func() tea.Msg {
		ctx := context.Background()
		warm, dead := m.sessions, m.dead
		if full {
			warm, dead = nil, nil
		}
		snap := m.app.Scan(ctx, warm, dead)
		if xs, e := m.app.DetectActive(ctx, snap.Sessions); e == nil {
			snap.Sessions = xs
		}
		// Mark empty, unstarted forks (e.g. codex/grok, which do not record a fork
		// point) so they can be excluded from the list. (For claude this is already
		// determined via ForkAt during Scan.)
		snap.Sessions = m.app.MarkEmptyForks(ctx, snap.Sessions)
		// A nil *cache.DB must not be handed over as the interface value: it would
		// be a non-nil ArtifactStore holding a nil pointer, and every call on it
		// would panic.
		var store app.ArtifactStore
		sessions := snap.Sessions
		if m.cache != nil {
			store = m.cache
			if warm, e := m.cache.Load(ctx); e == nil {
				sessions = app.MergeDeletedLogs(sessions, warm, scanSucceeded(m.app, snap))
			}
		}
		parents := m.sessionParents
		if parents == nil || !sameSessionParentTopology(m.sessions, sessions) {
			parents = m.app.LogicalSessionParents(ctx, sessions)
		}
		idx, fp := m.app.BuildIndex(ctx, sessions, store, m.index, m.indexFP)
		return scanMsg{snap: snap, sessions: sessions, parents: parents, index: idx, indexFP: fp}
	}
}

// sameSessionParentTopology reports whether the inputs that can change a
// canonical display parent are unchanged. Titles, timestamps and fingerprints
// deliberately do not participate: an established fork attachment does not
// move as its conversation grows, so periodic scans can reuse the resolved map.
func sameSessionParentTopology(a, b []domain.Session) bool {
	if len(a) != len(b) {
		return false
	}
	type relation struct {
		parent string
		empty  bool
	}
	relations := make(map[domain.SessionKey]relation, len(a))
	for _, s := range a {
		relations[s.Key()] = relation{parent: s.ParentSessionID, empty: s.EmptyFork}
	}
	for _, s := range b {
		if got, ok := relations[s.Key()]; !ok || got != (relation{parent: s.ParentSessionID, empty: s.EmptyFork}) {
			return false
		}
	}
	return true
}

func tick(d time.Duration) tea.Cmd {
	return tea.Tick(d, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func blinkTick() tea.Cmd {
	return tea.Tick(blinkInterval, func(t time.Time) tea.Msg { return blinkMsg(t) })
}
func (m *Model) filter() {
	m.filtered = nil
	q := strings.ToLower(m.query.Value())
	// firstSeen records the order in which each cwd first appears in the filtered
	// session order. It is the tie-breaker for folder-group ordering when the latest
	// timestamps are equal, preserving first-seen order via a stable sort.
	firstSeen := map[string]int{}
	// The query is read once, not once per session: splitting it into words is
	// the same work every time.
	query := searchpkg.NewQuery(q)
	for i, s := range m.sessions {
		if s.EmptyFork {
			continue // unstarted empty fork (same prefix as its parent): not shown in the list
		}
		if m.activeOnly && s.Status == "" {
			continue
		}
		if !query.Empty() && m.index != nil && !m.index.Match(s, query) {
			continue
		}
		if _, ok := firstSeen[s.CWD]; !ok {
			firstSeen[s.CWD] = len(firstSeen)
		}
		m.filtered = append(m.filtered, i)
	}
	if m.view == "folder" {
		latestByCWD := map[string]time.Time{}
		for _, ix := range m.filtered {
			s := m.sessions[ix]
			if latestByCWD[s.CWD].Before(s.UpdatedAt) {
				latestByCWD[s.CWD] = s.UpdatedAt
			}
		}
		sort.SliceStable(m.filtered, func(i, j int) bool {
			a, b := m.sessions[m.filtered[i]], m.sessions[m.filtered[j]]
			if a.CWD == b.CWD {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
			if !latestByCWD[a.CWD].Equal(latestByCWD[b.CWD]) {
				return latestByCWD[a.CWD].After(latestByCWD[b.CWD])
			}
			return firstSeen[a.CWD] < firstSeen[b.CWD]
		})
	} else {
		sort.SliceStable(m.filtered, func(i, j int) bool {
			a, b := m.sessions[m.filtered[i]], m.sessions[m.filtered[j]]
			if !a.UpdatedAt.Equal(b.UpdatedAt) {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
			if a.PluginID != b.PluginID {
				return a.PluginID < b.PluginID
			}
			return a.SessionID < b.SessionID
		})
	}
	m.rebuildRows()
}

// buildListRows builds the displayed / cursor-target rows from m.filtered (already
// sorted). Fork children are nested as a tree directly under their parent
// (appendForest). In folder view a header row is inserted whenever the cwd changes,
// and collapsed groups emit no session rows. Time view has no headers and treats
// everything as a single scope to forest.
func (m *Model) buildListRows() {
	m.rows = m.rows[:0]
	if m.view != "folder" {
		m.appendForest(m.filtered)
		return
	}
	// folder: filtered is sorted so identical CWDs are contiguous. Emit one header
	// plus a forest per CWD group.
	for i := 0; i < len(m.filtered); {
		cwd := m.sessions[m.filtered[i]].CWD
		var group []int
		for i < len(m.filtered) && m.sessions[m.filtered[i]].CWD == cwd {
			group = append(group, m.filtered[i])
			i++
		}
		m.rows = append(m.rows, listRow{Kind: "header", CWD: cwd, Collapsed: m.collapsed[cwd]})
		if !m.collapsed[cwd] {
			m.appendForest(group)
		}
	}
}

// appendForest arranges cand (session indices within one scope, already sorted in
// display order) into a parent/child tree and appends them to m.rows in pre-order.
// A child whose parent is outside cand (a parent in another scope, or filtered out)
// is treated as a root, preserving appearance order (UpdatedAt descending). Multi-level
// forks (a fork of a fork) are supported.
func (m *Model) appendForest(cand []int) {
	inScope := make(map[string]bool, len(cand))
	for _, ix := range cand {
		inScope[m.sessions[ix].SessionID] = true
	}
	children := map[string][]int{}
	var roots []int
	for _, ix := range cand {
		s := m.sessions[ix]
		parentID := m.sessionParentID(s)
		if parentID != "" && inScope[parentID] {
			children[parentID] = append(children[parentID], ix)
		} else {
			roots = append(roots, ix)
		}
	}
	// Sort roots and siblings in descending order by each node's subtree-latest time
	// (max UpdatedAt of itself plus its in-scope descendants). A tree's position is
	// decided by the newest node in it, not by any single node (the parent). The time
	// shown on each row remains the session's own UpdatedAt; only ordering changes here.
	latest := make(map[int]time.Time, len(cand))
	var calcLatest func(ix int) time.Time
	calcLatest = func(ix int) time.Time {
		if t, ok := latest[ix]; ok {
			return t // memoized; on a cycle this returns the settled value and avoids infinite recursion
		}
		t := m.sessions[ix].UpdatedAt
		latest[ix] = t
		for _, kid := range children[m.sessions[ix].SessionID] {
			if ct := calcLatest(kid); ct.After(t) {
				t = ct
			}
		}
		latest[ix] = t
		return t
	}
	for _, ix := range cand {
		calcLatest(ix)
	}
	// cand arrives in display order (UpdatedAt descending), so a stable sort preserves
	// the existing order within an equal subtree-latest.
	sortBySubtree := func(xs []int) {
		sort.SliceStable(xs, func(i, j int) bool { return latest[xs[i]].After(latest[xs[j]]) })
	}
	sortBySubtree(roots)
	for _, kids := range children {
		sortBySubtree(kids)
	}
	emitted := make(map[int]bool, len(cand))
	var dfs func(ix int, ancLast []bool, depth int, isLast bool)
	dfs = func(ix int, ancLast []bool, depth int, isLast bool) {
		if emitted[ix] {
			return // safety net against parent/child cycles (normally unreachable)
		}
		emitted[ix] = true
		m.rows = append(m.rows, listRow{
			Kind:       "session",
			CWD:        m.sessions[ix].CWD,
			SessIdx:    ix,
			Depth:      depth,
			TreePrefix: sessionTreePrefix(ancLast, isLast, depth, m.sessionParentID(m.sessions[ix])),
		})
		kids := children[m.sessions[ix].SessionID]
		var childAnc []bool
		if depth > 0 {
			childAnc = append(append([]bool{}, ancLast...), isLast)
		}
		for i, kid := range kids {
			dfs(kid, childAnc, depth+1, i == len(kids)-1)
		}
	}
	for _, r := range roots {
		dfs(r, nil, 0, true)
	}
	// Recover any rows missed (e.g. due to a cycle) by appending them at depth 0 so nothing is dropped.
	for _, ix := range cand {
		if !emitted[ix] {
			emitted[ix] = true
			m.rows = append(m.rows, listRow{Kind: "session", CWD: m.sessions[ix].CWD, SessIdx: ix})
		}
	}
}

// treePrefix builds the leading tree prefix from the depth, the last-child flag
// isLast, and the ancestors' last-child flags ancLast (for depths 1..depth-1).
// Depth 0 has no prefix.
func treePrefix(ancLast []bool, isLast bool, depth int) string {
	if depth == 0 {
		return ""
	}
	var b strings.Builder
	for _, last := range ancLast {
		if last {
			b.WriteString("   ")
		} else {
			b.WriteString("│  ")
		}
	}
	if isLast {
		b.WriteString("└─ ")
	} else {
		b.WriteString("├─ ")
	}
	return b.String()
}

// sessionTreePrefix caps visual indentation while preserving the immediate
// parent at deeper logical levels. At the cap, the remaining structural column
// is replaced by a lineage marker so every additional fork consumes the same
// horizontal space.
func sessionTreePrefix(ancLast []bool, isLast bool, depth int, parentSessionID string) string {
	if depth <= maxSessionTreeDepth {
		return treePrefix(ancLast, isLast, depth)
	}

	var b strings.Builder
	for i := 0; i < maxSessionTreeDepth-1; i++ {
		if i < len(ancLast) && !ancLast[i] {
			b.WriteString("│  ")
		} else {
			b.WriteString("   ")
		}
	}
	b.WriteString("↳")
	b.WriteString(shortID(parentSessionID))
	b.WriteString(" ")
	return b.String()
}

// rebuildRows rebuilds rows and clamps the cursor and scroll offset into range.
// Also used for operations that do not change the sort order m.filtered (collapse
// toggles, H/L).
func (m *Model) rebuildRows() {
	m.buildListRows()
	if m.cursor >= len(m.rows) {
		m.cursor = max(0, len(m.rows)-1)
	}
	m.ensureListOffset()
}

// Update calls the internal update and, if a new flash was set, arms an expiry
// timer. flash is set in many places, so rather than returning a timer from each of
// them we handle it once here.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	prev := m.flash
	model, cmd := m.update(msg)
	mm := model.(Model)
	// Refresh the derived search state once per update rather than per frame. Doing
	// it here instead of at each assignment to turnQuery means no key path can leave
	// the cache stale.
	(&mm).syncTurnHits()
	// If this update set a new flash (non-empty and different from before), record the
	// current time and arm an expiry timer. The expiry handler checks flashAt so an
	// older timer does not clear a newer message.
	if mm.flash != "" && mm.flash != prev {
		mm.flashAt = time.Now()
		return mm, tea.Batch(cmd, tea.Tick(flashTTL, func(time.Time) tea.Msg { return flashExpireMsg{} }))
	}
	return mm, cmd
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch x := msg.(type) {
	case tea.WindowSizeMsg:
		// How many lines the open summary wraps to depends on the width, so the
		// count from before the resize is what the cursor has to be moved against.
		headBefore := 0
		if m.detail != nil {
			headBefore = len(m.detailHeadLines(m.detailSession, -1))
		}
		m.width, m.height = x.Width, x.Height
		// Make the search input scroll horizontally rather than wrap. Without a width a
		// long query would wrap to a second line, push the body down by a row, and make
		// the number of visible sessions jitter as search is toggled on and off.
		m.query.Width = max(10, m.width-4)
		m.relocInput.Width = max(10, m.width-4)
		m.ensureListOffset()
		if m.detail != nil {
			(&m).reflowDetailRows(headBefore)
		}
		m.ensureDetailOffset()
		return m, nil
	case scanMsg:
		return m.handleScanMsg(x)
	case tickMsg:
		if !m.scanning {
			m.scanning = true
			return m, tea.Batch(m.scan(), tick(time.Duration(m.app.Config.UI.RescanInterval)))
		}
		return m, tick(time.Duration(m.app.Config.UI.RescanInterval))
	case blinkMsg:
		m.blinkOn = !m.blinkOn
		return m, blinkTick()
	case flashExpireMsg:
		// Clear the flash if flashTTL has elapsed. If it has not (a newer flash has
		// re-armed the timer), leave it and let that newer timer handle it.
		if m.flash != "" && !m.flashAt.IsZero() && time.Since(m.flashAt) >= flashTTL {
			m.flash = ""
			m.flashAt = time.Time{}
		}
		return m, nil
	case editorFinishedMsg:
		// A successful edit needs no announcement; the restored turn view is enough.
		if x.err != nil {
			m.flash = x.err.Error()
		}
		return m, nil
	case convMsg:
		return m.handleConvMsg(x)
	case tea.KeyMsg:
		return m.handleKey(x)
	}
	return m, nil
}

// handleScanMsg adopts the result of a background scan: it swaps in the new sessions,
// search index, and dead-set, persists and prunes the cache, refilters, and — if a
// detail view is open — reloads the conversation so it follows any growth.
func (m Model) handleScanMsg(x scanMsg) (tea.Model, tea.Cmd) {
	m.scanned = true
	m.sessions = x.sessions
	m.sessionParents = x.parents
	m.dead = x.snap.Dead
	m.index = x.index
	m.indexFP = x.indexFP
	if m.cache != nil {
		// The summary worker writes headlines while the TUI runs, so they are read
		// again with each scan rather than only at startup.
		m.headlines = m.cache.Headlines(context.Background())
		_ = m.cache.Save(context.Background(), x.snap.Sessions)
		successful := scanSucceeded(m.app, x.snap)
		_ = m.cache.Prune(context.Background(), x.snap.Sessions, successful, time.Duration(m.app.Config.Cache.MaxAge))
		_ = m.cache.Enforce(context.Background(), int64(m.app.Config.Cache.MaxSize))
		// The index that was just written is the only artifact worth keeping; a
		// previous generation of it would otherwise sit in the cache forever.
		_ = m.cache.DropSupersededArtifacts(context.Background(), app.SearchArtifactKind, app.ConversationArtifactKind)
	}
	m.scanning = false
	// The hook runs as a command rather than here: it parses conversations and
	// writes files, which at a scan every few seconds would stop the update loop
	// for as long as that takes. As a command it runs off the loop and returns
	// nothing, since it changes nothing on screen.
	var after tea.Cmd
	if m.afterScan != nil {
		hook, sessions := m.afterScan, x.snap.Sessions
		after = func() tea.Msg { hook(sessions); return nil }
	}
	m.filter()
	if m.detail != nil && m.detailSession != nil {
		key := m.detailSession.Key()
		for _, s := range m.sessions {
			if s.Key() == key {
				m.detailSession = &s
				return m, tea.Batch(after, m.loadConversation(s, false, false))
			}
		}
	}
	return m, after
}

// handleConvMsg applies a loaded conversation to the detail view.
func (m Model) handleConvMsg(x convMsg) (tea.Model, tea.Cmd) {
	// If the detail view was closed (Esc, etc.) right after being opened, detailSession
	// is nil again. A load result that arrives late must not set detail while
	// detailSession is nil, or detailView would dereference a nil *detailSession and
	// panic. When closed, drop the conversation body (but still surface an error flash).
	// A result for a *different* session (the view moved on while the load ran) is
	// stale and dropped entirely, so quickly closing A and opening B cannot show A's
	// conversation under B's header. (A zero key means the sender did not tag the
	// load; only tagged results are subject to the staleness check.)
	if m.detailSession != nil && x.key != (domain.SessionKey{}) && x.key != m.detailSession.Key() {
		return m, nil
	}
	// The summary is read again below and the worker rewrites it in the
	// background, so the head can be a different number of rows than the ones on
	// screen were built from. Everything under it moves by that difference.
	headBefore := len(m.detailHeadLines(m.detailSession, -1))
	if m.detailSession != nil {
		m.detail = x.c
		m.detailOrigins = x.origins
		m.summaries, m.summaryStale, m.summaryAt = nil, false, time.Time{}
		if m.cache != nil && x.c != nil {
			turns := transcript.Turns(*x.c, x.c.ActivePath())
			m.summaries = m.cache.Summaries(context.Background(), *m.detailSession, summary.NodesByTurn(turns))
			// Turn 0 alone can describe a session that has since gone on; what makes
			// it so is the store's business, not the view's (cache.Summary.Stale).
			m.summaryAt = m.summaries[0].Created
			m.summaryStale = m.summaries[0].Stale(*m.detailSession, m.summaryGrace())
		}
	}
	if x.c != nil && m.detail != nil {
		base := detailFrame{path: x.c.ActivePath(), label: "current"}
		if x.reset || len(m.detailPathStack) == 0 {
			m.detailPathStack = []detailFrame{base}
			// If the opened target is a non-root fork, descend into the exact path to
			// focusLeaf. DeepestPath can enter a newer child fork instead of stopping at
			// the session the user opened.
			if x.reset {
				if frame, ok := m.directFocusFrame(*x.c, x.focusLeaf); ok {
					m.detailPathStack = append(m.detailPathStack, frame)
				}
			}
		} else {
			// The base path always follows the conversation. A directly opened fork also
			// follows its latest focusLeaf, while frames entered manually remain pinned so
			// reload does not move the user away from a branch being inspected.
			frames := append([]detailFrame(nil), m.detailPathStack...)
			frames[0] = base
			for i := 1; i < len(frames); i++ {
				if !frames[i].followFocus {
					continue
				}
				if frame, ok := m.directFocusFrame(*x.c, x.focusLeaf); ok {
					frames[i] = frame
				}
			}
			m.detailPathStack = frames
		}
		m.setDetailPath(m.currentDetailPath())
		if x.reset {
			m.detailCursor = m.firstTurnRow()
			m.detailOffset = 0
			// The cursor no longer starts at the top — the head rows are above it —
			// so the window has to be brought to it. Without this the first frame
			// after opening is the head and none of the turns.
			(&m).ensureDetailOffset()
			m.turnOpen = false
			m.turnBlocks = nil
			m.turnExpanded = nil
			m.turnCursor = 0
			m.turnOffset = 0
		} else {
			// setDetailPath has already rebuilt the rows with the summary as it is
			// now, so only the cursor is left to move.
			(&m).shiftCursorForHead(headBefore)
			m.ensureDetailOffset()
			if m.turnOpen {
				m.openCurrentTurn(false)
			}
		}
	}
	if x.e != nil {
		m.flash = x.e.Error()
	}
	return m, nil
}

// handleKey routes a key press to the active mode: a pending action (confirm or
// relocate), the list search input, the detail view, or the list view.
func (m Model) handleKey(x tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.action != "" {
		return m.handleActionKey(x)
	}
	if m.searching {
		return m.handleSearchKey(x)
	}
	if m.detail != nil {
		return m.updateDetail(x)
	}
	return m.updateList(x)
}

// handleActionKey handles keys while a modal action is pending: the fork confirm
// prompt, the relocate flow, or the (rare) generic query-edit fallback.
func (m Model) handleActionKey(x tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.action == "confirm-fork" {
		if x.String() == "y" {
			if e := m.app.ApplyForkPlan(context.Background(), m.pendingPlan); e != nil {
				m.flash = e.Error()
			} else {
				c := m.pendingCmd
				m.launch = &c
				return m, tea.Quit
			}
		}
		m.action = ""
		return m, nil
	}
	if m.action == "relocate" || m.action == "relocate-confirm" {
		return m.updateRelocate(x)
	}
	if x.String() == "esc" {
		m.action = ""
		m.query.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.query, cmd = m.query.Update(x)
	return m, cmd
}

// handleSearchKey handles keys while the list search input is focused. Enter/Esc
// commit the query and leave search mode; any other key edits it and re-filters live.
func (m Model) handleSearchKey(x tea.KeyMsg) (tea.Model, tea.Cmd) {
	if x.String() == "enter" || x.String() == "esc" {
		m.searching = false
		m.query.Blur()
		m.filter()
		return m, nil
	}
	var cmd tea.Cmd
	m.query, cmd = m.query.Update(x)
	m.filter()
	return m, cmd
}

// updateDetail handles keys in the detail (turn list) view, delegating to the full
// turn view or the turn-list search input when one of those is active.
func (m Model) updateDetail(x tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.turnOpen {
		return m.updateTurnFull(x)
	}
	if m.turnSearching {
		return m.updateTurnListSearch(x)
	}
	switch x.String() {
	case "j", "down":
		if m.detailCursor+1 < len(m.detailRows) {
			m.detailCursor++
			m.ensureDetailOffset()
		}
		return m, nil
	case "k", "up":
		if m.detailCursor > 0 {
			m.detailCursor--
			m.ensureDetailOffset()
		}
		return m, nil
	case "pgdown":
		m.detailCursor = min(max(0, len(m.detailRows)-1), m.detailCursor+10)
		m.ensureDetailOffset()
		return m, nil
	case "pgup":
		m.detailCursor = max(0, m.detailCursor-10)
		m.ensureDetailOffset()
		return m, nil
	case "g":
		m.detailCursor = 0
		m.ensureDetailOffset()
		return m, nil
	case "G":
		m.detailCursor = max(0, len(m.detailRows)-1)
		m.ensureDetailOffset()
		return m, nil
	case "/":
		m.turnSearching = true
		m.turnQuery = ""
		m.turnSearchPos = -1
		return m, nil
	case "n":
		(&m).jumpTurnHit(true)
		return m, nil
	case "N":
		(&m).jumpTurnHit(false)
		return m, nil
	case "o":
		if m.detailSession != nil {
			if c, e := m.app.ResumeCommand(*m.detailSession); e != nil {
				m.flash = e.Error()
			} else {
				m.launch = &c
				return m, tea.Quit
			}
		}
		return m, nil
	case "c":
		if m.detailSession != nil {
			if c, e := m.app.ShellCommand(m.detailSession.CWD); e != nil {
				m.flash = e.Error()
			} else {
				m.launch = &c
				return m, tea.Quit
			}
		}
		return m, nil
	case "s":
		return m.toggleSummary()
	case "y":
		return m.copyTurnExchange()
	case "x":
		return m.exportSession()
	case "enter", "right", "l":
		if row, ok := m.selectedDetailRow(); ok {
			switch row.Kind {
			case "head", "headtext":
				// Enter on any line of the head opens or closes it, so a row that
				// grew out of the summary folds back the way it came.
				return m.toggleSummary()
			case "forkparent":
				return m.jumpToParent()
			case "branch":
				if child, ok := m.forkBranchSession(row.Root); ok {
					m.forkBack = nil
					return m, m.openSessionDetail(child, m.turnQuery)
				}
				m.detailPathStack = append(m.detailPathStack, detailFrame{path: convlogic.DeepestPath(*m.detail, row.Root), label: m.branchFrameLabel(row.Root)})
				m.setDetailPath(m.currentDetailPath())
				m.detailCursor = m.firstTurnRow()
				m.detailOffset = 0
				(&m).ensureDetailOffset()
				return m, nil
			}
		}
		m.openCurrentTurn(true)
		return m, nil
	}
	if x.String() == "f" && m.detailSession != nil {
		return m.forkFromDetail()
	}
	if x.String() == "p" && m.detailSession != nil {
		return m.jumpToParent()
	}
	if x.String() == "q" || x.String() == "left" || x.String() == "esc" || x.String() == "h" {
		return m.detailBack(x)
	}
	return m, nil
}

// firstTurnRow is where the cursor lands when a session or a branch is opened:
// the newest turn, not the head row above it. What the session was last doing is
// what the reader came for, and the head is one `k` away.
func (m Model) firstTurnRow() int {
	for i, r := range m.detailRows {
		if r.Kind == "turn" {
			return i
		}
	}
	return 0
}

// toggleSummary opens or closes the head rows: closed they are one line holding
// the session's own summary, open they are the title in full and the whole
// summary under it. With no summary there is nothing to open.
//
// The rows below shift by however many lines that adds or removes, so the cursor
// moves with them and stays on what it was on.
func (m Model) toggleSummary() (tea.Model, tea.Cmd) {
	if !m.hasSessionSummary() {
		return m, nil
	}
	before := len(m.detailHeadLines(m.detailSession, -1))
	m.summaryOpen = !m.summaryOpen
	(&m).reflowDetailRows(before)
	return m, nil
}

// reflowDetailRows rebuilds the rows after something changed how many lines the
// head takes — opening it, or a resize that rewraps it — and moves the cursor by
// as much, so it stays on the row it was on rather than on whatever slid into
// that position.
func (m *Model) reflowDetailRows(before int) {
	m.rebuildDetailRows(m.currentDetailPath())
	m.shiftCursorForHead(before)
	m.ensureDetailOffset()
}

// shiftCursorForHead moves the cursor by however many lines the head gained or
// lost, so it keeps pointing at the row it pointed at. The rows must already be
// rebuilt: what the cursor is clamped to is their new length.
func (m *Model) shiftCursorForHead(before int) {
	after := len(m.detailHeadLines(m.detailSession, -1))
	if m.detailCursor >= before {
		m.detailCursor += after - before
	} else {
		// The cursor was inside the head itself, and the head just got shorter than
		// where it was standing.
		m.detailCursor = min(m.detailCursor, max(0, after-1))
	}
	m.detailCursor = max(0, min(m.detailCursor, len(m.detailRows)-1))
}

// updateTurnFull handles keys in the full (single-turn) view, including its own inline
// search input.
func (m Model) updateTurnFull(x tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.turnFullSearching {
		switch x.Type {
		case tea.KeyEnter:
			m.turnFullSearching = false
			(&m).jumpTurnFullHit(true)
			return m, nil
		case tea.KeyEsc:
			m.turnFullSearching = false
			return m, nil
		case tea.KeyBackspace:
			m.turnFullQuery = dropLastRune(m.turnFullQuery)
			(&m).applyTurnFullSearch()
			return m, nil
		case tea.KeyRunes, tea.KeySpace:
			m.turnFullQuery += string(x.Runes)
			(&m).applyTurnFullSearch()
			(&m).jumpTurnFullHit(true)
			return m, nil
		}
		if x.String() == "ctrl+u" {
			m.turnFullQuery = ""
		}
		return m, nil
	}
	switch x.String() {
	case "j", "down":
		if m.turnCursor+1 < len(m.turnFullLines()) {
			m.turnCursor++
			m.ensureTurnOffset()
		}
		return m, nil
	case "k", "up":
		if m.turnCursor > 0 {
			m.turnCursor--
			m.ensureTurnOffset()
		}
		return m, nil
	case "e":
		// "o" already means resume on the list screen, so edit-in-editor takes "e".
		// Only the edited-files blocks stand for a file; elsewhere "e" does nothing.
		if blk := m.blockAtCursor(); blk >= 0 && m.turnBlocks[blk].Path != "" {
			return m, openInEditor(m.turnBlocks[blk].Path)
		}
		return m, nil
	case "pgdown":
		m.turnCursor = min(max(0, len(m.turnFullLines())-1), m.turnCursor+15)
		m.ensureTurnOffset()
		return m, nil
	case "pgup":
		m.turnCursor = max(0, m.turnCursor-15)
		m.ensureTurnOffset()
		return m, nil
	case "g":
		m.turnCursor = 0
		m.ensureTurnOffset()
		return m, nil
	case "G":
		m.turnCursor = max(0, len(m.turnFullLines())-1)
		m.ensureTurnOffset()
		return m, nil
	case "tab", "]":
		(&m).jumpTurnBlock(1)
		return m, nil
	case "shift+tab", "[":
		(&m).jumpTurnBlock(-1)
		return m, nil
	case "/":
		m.turnFullSearching = true
		m.turnFullQuery = ""
		return m, nil
	case "n":
		(&m).jumpTurnFullHit(true)
		return m, nil
	case "N":
		(&m).jumpTurnFullHit(false)
		return m, nil
	case "enter", "right", "l", " ":
		m.toggleTurnBlock(m.blockAtCursor())
		return m, nil
	case "z":
		m.toggleAllTurnBlocks()
		return m, nil
	case "y":
		return m.copyTurnBlock()
	case "esc":
		if m.turnFullQuery != "" {
			m.turnFullQuery = ""
			return m, nil
		}
		m.turnOpen = false
		m.turnFullQuery = ""
		return m, nil
	case "q", "left", "h":
		// ← / h first fold the expanded block under the cursor (mirroring → which
		// expands it), parking the cursor on its header; q, or a second press, leaves.
		if blk := m.blockAtCursor(); x.String() != "q" && m.turnExpanded[blk] {
			m.toggleTurnBlock(blk)
			if line := m.turnBlockHeaderLine(blk); line >= 0 {
				m.turnCursor = line
				m.ensureTurnOffset()
			}
			return m, nil
		}
		m.turnOpen = false
		m.turnFullQuery = ""
		return m, nil
	}
	return m, nil
}

// updateTurnListSearch handles keys while the turn-list search input is focused.
func (m Model) updateTurnListSearch(x tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch x.Type {
	case tea.KeyEnter:
		m.turnSearching = false
		(&m).jumpTurnHit(true)
		return m, nil
	case tea.KeyEsc:
		m.turnSearching = false
		return m, nil
	case tea.KeyBackspace:
		m.turnQuery = dropLastRune(m.turnQuery)
		m.turnSearchPos = -1
		return m, nil
	case tea.KeyRunes, tea.KeySpace:
		m.turnQuery += string(x.Runes)
		m.turnSearchPos = -1
		return m, nil
	}
	if x.String() == "ctrl+u" {
		m.turnQuery = ""
		m.turnSearchPos = -1
	}
	return m, nil
}

// forkFromDetail creates a fork from the selected turn (or from the active leaf when
// the cursor is not on a turn) and arms the confirm prompt. The displayed tree is
// synthesized (fork-child nodes carry grafted IDs that don't exist in any file), so
// the selected node is translated back to its owning session and real node ID via
// detailOrigins; app.ForkAt then recomputes KeepTurns in the owner's own numbering.
func (m Model) forkFromDetail() (tea.Model, tea.Cmd) {
	target := m.detail.ActiveLeaf
	if row, ok := m.selectedDetailRow(); ok && row.Kind == "turn" && len(row.Turn) > 0 {
		turn := row.Turn
		target = turn[len(turn)-1]
	}
	origin, ok := m.detailOrigins[target]
	if !ok {
		// No graft mapping for this node: it can only belong to the focused session.
		origin = app.NodeOrigin{Session: *m.detailSession, NodeID: target}
	}
	plan, cmd, e := m.app.ForkAt(context.Background(), origin)
	if e != nil {
		m.flash = e.Error()
	} else {
		m.pendingPlan, m.pendingCmd = plan, cmd
		m.action = "confirm-fork"
	}
	return m, nil
}

// jumpToParent navigates from a fork to its parent session, remembering the child so
// "back" can return to it.
func (m Model) jumpToParent() (tea.Model, tea.Cmd) {
	pid := m.sessionParentID(*m.detailSession)
	if pid == "" {
		m.flash = "This session has no parent"
		return m, nil
	}
	var parent *domain.Session
	for i := range m.sessions {
		// Match the plugin too: session IDs are only unique within one plugin.
		if m.sessions[i].PluginID == m.detailSession.PluginID && m.sessions[i].SessionID == pid {
			parent = &m.sessions[i]
			break
		}
	}
	if parent == nil {
		m.flash = fmt.Sprintf("Parent %s not found (not loaded/deleted)", shortID(pid))
		return m, nil
	}
	m.forkBack = append(m.forkBack, *m.detailSession)
	m.flash = fmt.Sprintf("Jumped to parent: %s (q to go back)", shortID(pid))
	return m, m.openSessionDetail(*parent, "")
}

// detailBack handles the "go back" keys (q / ← / Esc / h) in the detail view, peeling
// off one navigation layer at a time: a live search query, a descended branch frame, a
// fork-parent jump, and finally closing the detail view entirely.
func (m Model) detailBack(x tea.KeyMsg) (tea.Model, tea.Cmd) {
	// If a search query remains, clear it first (the step before going back).
	if x.String() == "esc" && m.turnQuery != "" {
		m.turnQuery = ""
		m.turnSearchPos = -1
		return m, nil
	}
	if len(m.detailPathStack) > 1 {
		m.detailPathStack = m.detailPathStack[:len(m.detailPathStack)-1]
		m.setDetailPath(m.currentDetailPath())
		m.detailCursor = m.firstTurnRow()
		m.detailOffset = 0
		(&m).ensureDetailOffset()
		return m, nil
	}
	// If we had jumped to the fork's parent, "back" returns to the fork child.
	if len(m.forkBack) > 0 {
		child := m.forkBack[len(m.forkBack)-1]
		m.forkBack = m.forkBack[:len(m.forkBack)-1]
		m.flash = "" // the "Jumped to parent" notice is no longer valid once we return to the child
		return m, m.openSessionDetail(child, "")
	}
	m.detail = nil
	m.detailSession = nil
	m.turnOpen = false
	m.detailPathStack = nil
	m.turnQuery = ""
	m.turnSearchPos = -1
	m.flash = "" // do not carry the detail-view notice (parent jump, etc.) over to the list
	return m, nil
}

// updateList handles keys in the top-level session list view.
func (m Model) updateList(x tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch x.String() {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "/":
		m.searching = true
		m.query.Focus()
		return m, textinput.Blink
	case "esc":
		// Esc after committing: clear the filter (Esc during input is treated as a commit
		// in handleSearchKey).
		if m.query.Value() != "" {
			m.query.SetValue("")
			m.cursor = 0
			m.offset = 0
			m.filter()
		}
		return m, nil
	case "j", "down":
		if m.cursor+1 < len(m.rows) {
			m.cursor++
			m.ensureListOffset()
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
			m.ensureListOffset()
		}
	case "g":
		m.cursor = 0
		m.ensureListOffset()
	case "G":
		m.cursor = max(0, len(m.rows)-1)
		m.ensureListOffset()
	case "s":
		// Only when there is one to swap: with no summaries yet both columns are
		// the title and the key would read as broken.
		if len(m.headlines) > 0 {
			m.listTitles = !m.listTitles
		}
		return m, nil
	case "a":
		// Toggle the active filter (orthogonal to the view). Row layout changes, so reset the position.
		m.activeOnly = !m.activeOnly
		m.cursor = 0
		m.offset = 0
		m.filter()
	case "r":
		// Ignore while a scan is in flight: a second concurrent scan could land
		// out of order and overwrite newer results with older ones (and would
		// hit the cache DB concurrently).
		if m.scanning {
			return m, nil
		}
		m.scanning = true
		return m, m.scan()
	case "v":
		// Switch view: time order <-> folder. Row layout changes, so reset the position.
		if m.view == "time" {
			m.view = "folder"
		} else {
			m.view = "time"
		}
		m.cursor = 0
		m.offset = 0
		m.filter()
	case "H":
		// Collapse all groups (folder-view open/close; the state is kept even in time view).
		m.collapsed = map[string]bool{}
		for _, s := range m.sessions {
			m.collapsed[s.CWD] = true
		}
		m.rebuildRows()
	case "L":
		// Expand all groups.
		m.collapsed = map[string]bool{}
		m.rebuildRows()
	case "enter", "right", "l":
		// A header row toggles open/close; a session row opens the detail view.
		if m.cursor >= 0 && m.cursor < len(m.rows) && m.rows[m.cursor].Kind == "header" {
			cwd := m.rows[m.cursor].CWD
			if m.collapsed == nil {
				m.collapsed = map[string]bool{}
			}
			if m.collapsed[cwd] {
				delete(m.collapsed, cwd)
			} else {
				m.collapsed[cwd] = true
			}
			m.rebuildRows()
			return m, nil
		}
		if s, ok := m.selected(); ok {
			m.forkBack = nil
			// Carry the list's search query over to the turn list.
			return m, m.openSessionDetail(s, m.query.Value())
		}
	case "o":
		if s, ok := m.selected(); ok {
			if c, e := m.app.ResumeCommand(s); e != nil {
				m.flash = e.Error()
			} else {
				m.launch = &c
				return m, tea.Quit
			}
		} else if m.cursor >= 0 && m.cursor < len(m.rows) {
			m.flash = "Select a session row to resume (Enter toggles a header)"
		}
	case "c":
		if cwd, ok := m.selectedCWD(); ok {
			if c, e := m.app.ShellCommand(cwd); e != nil {
				m.flash = e.Error()
			} else {
				m.launch = &c
				return m, tea.Quit
			}
		}
	case "m":
		return m.startRelocate()
	}
	return m, nil
}

// startRelocate begins relocating the selected cwd's group to another path (a header
// targets its group, a session row targets its cwd). It refuses when the group has a
// live session, and is read-only when no plugin in the group supports relocation.
func (m Model) startRelocate() (tea.Model, tea.Cmd) {
	cwd, ok := m.selectedCWD()
	if !ok {
		return m, nil
	}
	n, live, relocatable := 0, 0, false
	for _, s := range m.sessions {
		if s.CWD != cwd {
			continue
		}
		n++
		if s.Status != "" {
			live++
		}
		if p, ok := m.app.Catalog.Plugin(s.PluginID); ok && p.Descriptor.Capabilities.Relocate {
			relocatable = true
		}
	}
	switch {
	case n == 0:
		// nothing to do
	case !relocatable:
		m.flash = "sessions are read-only; relocate is unavailable"
	case live > 0:
		m.flash = fmt.Sprintf("not relocating: project has %d live session(s)", live)
	default:
		m.action = "relocate"
		m.relocOld = cwd
		m.relocCycle = nil
		m.relocCands = nil
		m.relocInput.SetValue(cwd)
		m.relocInput.CursorEnd()
		m.relocInput.Focus()
		return m, textinput.Blink
	}
	return m, nil
}
func (m Model) selected() (domain.Session, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return domain.Session{}, false
	}
	r := m.rows[m.cursor]
	if r.Kind != "session" {
		return domain.Session{}, false
	}
	return m.sessions[r.SessIdx], true
}

// selectedCWD returns the cwd of the selected row (a header's group cwd, or a session's cwd).
func (m Model) selectedCWD() (string, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return "", false
	}
	return m.rows[m.cursor].CWD, true
}

// openSessionDetail switches the detail view to s and starts the same canonical
// conversation load regardless of whether s was selected from the session list,
// a fork row, or parent/back navigation.
func (m *Model) openSessionDetail(s domain.Session, query string) tea.Cmd {
	if m.focusLoadedSessionDetail(s, query) {
		return nil
	}
	m.detailSession = &s
	m.detail = nil
	m.detailOrigins = nil
	m.detailTurns = nil
	m.detailRows = nil
	m.detailPathStack = nil
	m.detailActive = nil
	m.detailNewestChron = -1
	m.detailCursor = 0
	m.detailOffset = 0
	m.summaries = nil
	m.summaryStale = false
	m.summaryAt = time.Time{}
	m.turnOpen = false
	m.turnBlocks = nil
	m.turnExpanded = nil
	m.turnCursor = 0
	m.turnOffset = 0
	m.resetDetailSearch(query)
	return m.loadConversation(s, true, true)
}

// focusLoadedSessionDetail switches session context using the canonical tree
// already in memory. Fork rows, parent navigation and back therefore do not
// parse the same session files again.
func (m *Model) focusLoadedSessionDetail(s domain.Session, query string) bool {
	if m.detail == nil {
		return false
	}
	focusLeaf := m.loadedSessionFocusLeaf(s)
	if focusLeaf == "" {
		return false
	}

	c, origins := m.detail, m.detailOrigins
	m.detailSession = &s
	m.resetDetailSearch(query)
	updated, _ := m.handleConvMsg(convMsg{c: c, focusLeaf: focusLeaf, origins: origins, key: s.Key(), reset: true})
	*m = updated.(Model)
	return true
}

func (m Model) loadedSessionFocusLeaf(s domain.Session) string {
	for id, origin := range m.detailOrigins {
		if origin.ActiveLeaf && origin.Session.Key() == s.Key() {
			return id
		}
	}
	return ""
}

func (m *Model) resetDetailSearch(query string) {
	m.turnQuery = strings.TrimSpace(query)
	m.turnSearching = false
	m.turnSearchPos = -1
	m.turnHits = nil
	m.turnHitsQuery = ""
	m.turnHitsStale = true
	m.turnFullSearching = false
	m.turnFullQuery = ""
}

func (m Model) loadConversation(s domain.Session, useCache, reset bool) tea.Cmd {
	return func() tea.Msg {
		ctx := context.Background()
		_ = useCache
		// Build the whole tree from s's root ancestor rather than from s itself, so that
		// however a session is opened it canonicalizes to the same parent → … → current
		// tree. focusLeaf points the view at s's own branch.
		c, focusLeaf, origins, e := m.app.ConversationFromFocus(ctx, s, m.sessions)
		return convMsg{c: c, focusLeaf: focusLeaf, origins: origins, key: s.Key(), e: e, reset: reset}
	}
}
func fit(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return runewidth.Truncate(s, n, "…")
}
func clip(s string, n int) string { return runewidth.Truncate(s, n, "") }

// wrapWidth hard-wraps s to display width n, splitting it so each segment's display
// width is at most n. Wide (full-width) characters are counted by their runewidth, so
// mixed-width text is never split mid-cell. An empty string is returned as a single
// empty line (to preserve separator rows). n<=0 returns s unchanged (to avoid an
// infinite loop).
func wrapWidth(s string, n int) []string {
	if n <= 0 || runewidth.StringWidth(s) <= n {
		return []string{s}
	}
	var out []string
	var cur strings.Builder
	w := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > n {
			out = append(out, cur.String())
			cur.Reset()
			w = 0
		}
		cur.WriteRune(r)
		w += rw
	}
	out = append(out, cur.String())
	return out
}

// statusMark labels what a session is doing, which is what LastKind reports:
// plugins whose logs only record a block once it is finished map the tail
// through common.Ongoing first, so a kind here means the activity, not the
// record. THINK is therefore the model working before it emits anything
// (a prompt or a tool result went in), and a session already producing its
// answer — reasoning over, text or a tool input streaming out — is RUN.
func statusMark(s domain.Session) string {
	if s.Status == domain.StatusRunning {
		if s.PermissionWait {
			return padCol("● ASK", 8)
		}
		if s.LastKind == domain.EventToolCall {
			return padCol("● TOOL", 8)
		}
		// EventQueued is a prompt sent while the turn was still running: like a plain prompt, it
		// means the model has it and has not answered yet.
		if s.LastKind == domain.EventUser || s.LastKind == domain.EventQueued || s.LastKind == domain.EventReasoning || s.LastKind == domain.EventToolResult {
			return padCol("● THINK", 8)
		}
		return padCol("● RUN", 8)
	}
	if s.Status == domain.StatusReady {
		return padCol("○ READY", 8)
	}
	if s.Status == domain.StatusOther {
		return padCol("· OTHER", 8)
	}
	return strings.Repeat(" ", 8)
}

// plural renders a count with its unit, adding the "s" when there is not exactly
// one ("1 turn", "3 turns").
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
func padCol(s string, w int) string {
	s = runewidth.Truncate(s, w, "")
	return s + strings.Repeat(" ", max(0, w-runewidth.StringWidth(s)))
}
func shortID(s string) string {
	if s == "" {
		return "????????"
	}
	r := []rune(s)
	if len(r) > 8 {
		r = r[:8]
	}
	return string(r)
}
func shortCWD(s string, w int) string {
	if h, err := os.UserHomeDir(); err == nil {
		if s == h {
			s = "~"
		} else if strings.HasPrefix(s, h+string(os.PathSeparator)) {
			s = "~" + strings.TrimPrefix(s, h)
		}
	}
	if runewidth.StringWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return runewidth.Truncate(s, w, "")
	}
	left := (w - 1 + 1) / 2
	right := w - 1 - left
	return runewidth.Truncate(s, left, "") + "…" + tailWidth(s, right)
}

// absCWD resolves a change's path against the session's working directory.
// Plugins report a mix of absolute and cwd-relative paths; opening a file needs
// the absolute one. A relative path with no cwd to anchor it stays as it is, and
// fails the existence check later rather than resolving against agentcarto's own
// working directory, which has nothing to do with the session.
func absCWD(path, cwd string) string {
	if path == "" || filepath.IsAbs(path) || cwd == "" {
		return path
	}
	return filepath.Join(cwd, path)
}
func tailWidth(s string, w int) string {
	r := []rune(s)
	out := ""
	for i := len(r) - 1; i >= 0; i-- {
		x := string(r[i]) + out
		if runewidth.StringWidth(x) > w {
			break
		}
		out = x
	}
	return out
}

var bars = []rune("▁▂▃▄▅▆▇█")

func countBar(n, maxN int) string {
	if n <= 0 {
		return " "
	}
	if maxN <= 1 {
		return string(bars[0])
	}
	f := math.Log(float64(n+1)) / math.Log(float64(maxN+1))
	i := int(math.Round(f * float64(len(bars)-1)))
	return string(bars[min(len(bars)-1, max(0, i))])
}
func colorName(name string) lipgloss.Color {
	m := map[string]string{"black": "0", "red": "1", "green": "2", "yellow": "3", "blue": "4", "magenta": "5", "cyan": "6", "white": "7", "orange": "208", "bright-black": "8", "bright-red": "9", "bright-green": "10", "bright-yellow": "11", "bright-blue": "12", "bright-magenta": "13", "bright-cyan": "14", "bright-white": "15"}
	if x := m[name]; x != "" {
		return lipgloss.Color(x)
	}
	return lipgloss.Color("")
}

// pluginColor colors a session by its plugin's configured color (config.yaml
// carries per-agent defaults); a session whose plugin is gone falls back to a
// neutral color.
func (m Model) pluginColor(s domain.Session) lipgloss.Color {
	if m.app != nil {
		if p, ok := m.app.Catalog.Plugin(s.PluginID); ok {
			if c := colorName(p.Color); c != "" {
				return c
			}
		}
	}
	return lipgloss.Color("7")
}
func statusColor(s domain.Session) lipgloss.Color {
	if s.PermissionWait && s.Status == domain.StatusRunning {
		return lipgloss.Color("1")
	}
	if s.Status == domain.StatusRunning {
		return lipgloss.Color("3")
	}
	if s.Status == domain.StatusReady {
		return lipgloss.Color("2")
	}
	return lipgloss.Color("")
}
func styled(text string, fg lipgloss.Color, selected, bold bool) string {
	st := lipgloss.NewStyle().Bold(bold)
	if fg != "" {
		st = st.Foreground(fg)
	}
	if selected {
		st = st.Background(lipgloss.Color("238"))
	}
	return st.Render(text)
}

// statusStyled renders the status mark. An ask-wait (PermissionWait) is drawn as bold
// white text on a statusColor background so it stands out regardless of selection.
func statusStyled(text string, s domain.Session, selected, bold bool) string {
	if s.PermissionWait && s.Status == domain.StatusRunning {
		return lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("15")).
			Background(statusColor(s)).
			Render(text)
	}
	return styled(text, statusColor(s), selected, bold)
}
func footer(text string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("7")).Render(text)
}

// flashBar colors a transient message (flash) so it is distinct from the normal footer
// (black on white). For emphasis it uses bold black text on a yellow background (yellow
// rather than red, which would be too strong; black has better contrast on yellow than
// white).
func flashBar(text string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("0")).Background(lipgloss.Color("3")).Bold(true).Render(text)
}
func roleColor(role string) lipgloss.Color {
	switch role {
	case "assistant", "task":
		return lipgloss.Color("6")
	case "ask", "del":
		return lipgloss.Color("1")
	case "badge":
		return lipgloss.Color("5")
	case "meta", "ready":
		return lipgloss.Color("3")
	case "folder", "tool":
		return lipgloss.Color("4")
	case "user":
		return lipgloss.Color("7")
	case "running", "add":
		return lipgloss.Color("2")
	}
	return lipgloss.Color("")
}
func (m Model) sessionRow(s domain.Session, selected bool, maxN int, prefix string) string {
	count := 0
	known := false
	if m.index != nil {
		count, known = m.index.Count(s)
	}
	optsCWD := m.view == "time" && m.width >= 93
	optsID := m.width >= 62
	optsCount := m.width >= 53
	var b strings.Builder
	b.WriteString(styled(" ", "", selected, false))
	b.WriteString(statusStyled(statusMark(s), s, selected, s.Status == domain.StatusRunning || s.Status == domain.StatusReady))
	b.WriteString(styled(s.UpdatedAt.Local().Format("01-02 15:04")+" ", "", selected, false))
	b.WriteString(styled(fmt.Sprintf("%-12s ", s.AgentType), m.pluginColor(s), selected, false))
	if optsCount {
		bar, num := " ", ""
		if known {
			bar, num = countBar(count, maxN), fmtCount(count)
		}
		b.WriteString(styled(fmt.Sprintf("%s %4s ", bar, num), lipgloss.Color("2"), selected, false))
	}
	if optsID {
		b.WriteString(styled(shortID(s.SessionID)+" ", lipgloss.Color("3"), selected, false))
	}
	if optsCWD {
		b.WriteString(styled(padCol(shortCWD(s.CWD, 30), 30)+" ", lipgloss.Color("4"), selected, true))
	}
	// A fork child whose parent is not shown in the tree (a root row) indicates its
	// parent with an ↳ lineage marker. Already-nested rows (prefix != "") show their
	// parent via the tree, so we omit the redundant marker.
	if parentID := m.sessionParentID(s); parentID != "" && prefix == "" {
		b.WriteString(styled("↳"+shortID(parentID)+" ", lipgloss.Color("5"), selected, false))
	}
	// Nested fork rows put the tree prefix at the start of the Title column. The fixed
	// columns (status/date/id, etc.) stay aligned across all rows regardless of depth,
	// and the hierarchy is shown by the ├─/└─ at the start of the title. Inserting the
	// prefix after the status column would shift the following columns by the depth and
	// break alignment, so it is placed here.
	if prefix != "" {
		b.WriteString(styled(prefix, lipgloss.Color("5"), selected, false))
	}
	// The log this row was read from is gone; what is shown came from the cache.
	// It matters before anything is asked of the session: resume and fork are not
	// offered for it.
	if s.LogDeleted {
		b.WriteString(styled("⌫ ", lipgloss.Color("8"), selected, false))
	}
	remain := max(1, m.width-1-lipgloss.Width(b.String()))
	// The title is the session's first prompt; the headline is what an agent made
	// of the whole session. `s` swaps between them, and a session with no headline
	// yet keeps its title rather than an empty column.
	//
	// The column is not colored to say which it is: the header names the mode, and
	// the color that would mark generated text here (meta) is the one the session
	// id already uses two columns over.
	lead := s.Title
	if !m.listTitles {
		if h := m.headlines[s.Key()]; strings.TrimSpace(h.Headline) != "" {
			lead = strings.TrimSpace(h.Headline)
			if h.Stale(s, m.summaryGrace()) {
				// The same mark the detail header uses, for the same reason: this is
				// the screen where a session is chosen, and one described as it was
				// this morning must not read as current.
				lead = "(stale) " + lead
			}
		}
	}
	b.WriteString(styled(clip(lead, remain), "", selected, false))
	line := b.String()
	plainW := lipgloss.Width(line)
	if selected && plainW < m.width-1 {
		line += styled(strings.Repeat(" ", m.width-1-plainW), "", true, false)
	}
	return line
}
func fmtCount(n int) string {
	if n >= 10000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}

// folderHeaderRow renders the cwd group header in folder view (arrow ▶/▼, path, count,
// and per-agent breakdown). The count and breakdown cover the whole group (all rows in
// m.filtered with the same cwd) regardless of collapse state.
func (m Model) folderHeaderRow(r listRow, selected bool) string {
	count := 0
	mix := map[string]int{}
	for _, ix := range m.filtered {
		g := m.sessions[ix]
		if g.CWD == r.CWD {
			count++
			mix[g.AgentType]++
		}
	}
	agents := make([]string, 0, len(mix))
	for a := range mix {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	parts := []string{}
	for _, a := range agents {
		parts = append(parts, fmt.Sprintf("%s×%d", a, mix[a]))
	}
	arrow := "▼"
	if r.Collapsed {
		arrow = "▶"
	}
	countS := fmt.Sprintf("(%d)", count)
	if len(parts) > 0 {
		countS = fmt.Sprintf("(%d: %s)", count, strings.Join(parts, " "))
	}
	h := fmt.Sprintf("%s %s  %s", arrow, shortCWD(r.CWD, max(10, m.width-25)), countS)
	line := styled(clip(h, max(20, m.width-1)), lipgloss.Color("4"), selected, true)
	// When selected, fill to the end of the line with the selection background (same as
	// session rows). Without this the header's selection highlight would stop at the text
	// width, making it look "not selected" even with the cursor on it.
	if selected {
		if plainW := lipgloss.Width(line); plainW < m.width-1 {
			line += styled(strings.Repeat(" ", m.width-1-plainW), "", true, false)
		}
	}
	return line
}

// endRelocate ends the relocate input. It does not touch the search query, so the
// pre-relocate filter state (m.query and m.rows) is left intact.
func (m Model) endRelocate() Model {
	m.action = ""
	m.relocOld = ""
	m.relocCount = 0
	m.relocCycle = nil
	m.relocCands = nil
	m.relocInput.Blur()
	m.relocInput.SetValue("")
	return m
}

// updateRelocate handles keys for the two relocate phases (input and confirm).
func (m Model) updateRelocate(x tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.action == "relocate-confirm" {
		if s := x.String(); s == "y" || s == "Y" {
			r, e := m.app.Relocate(context.Background(), m.relocOld, m.relocInput.Value(), m.sessions)
			if e != nil {
				m.flash = e.Error()
			} else {
				m.flash = fmt.Sprintf("relocated %d items", len(r.Completed))
			}
			m = m.endRelocate()
			// Mark the rescan as in flight so the next tick doesn't start a second one.
			m.scanning = true
			return m, m.scan()
		}
		m = m.endRelocate()
		return m, nil
	}
	switch x.String() {
	case "enter":
		// While the candidate list is shown, Enter first accepts the candidate, collapses
		// the list, and continues editing (like zsh menu-accept). Enter advances to the
		// confirm phase only when no list is shown.
		if len(m.relocCands) > 0 {
			m.relocCycle, m.relocCands = nil, nil
			return m, nil
		}
		newPath := normalizeRelocPath(m.relocInput.Value())
		msg, ok, cancel := validateRelocPath(newPath, m.relocOld)
		if cancel {
			m.flash = msg
			m = m.endRelocate()
			return m, nil
		}
		if !ok {
			m.flash = msg
			return m, nil
		}
		m.relocInput.SetValue(newPath)
		m.relocCount = 0
		for _, s := range m.sessions {
			if s.CWD == m.relocOld {
				m.relocCount++
			}
		}
		m.action = "relocate-confirm"
		return m, nil
	case "esc":
		m = m.endRelocate()
		return m, nil
	case "tab":
		m = m.relocateTab()
		return m, nil
	default:
		// Editing keys (characters, Backspace, Ctrl+u, etc.) are delegated to textinput and
		// clear the Tab-completion cycle and candidate display.
		m.relocCycle, m.relocCands = nil, nil
		var cmd tea.Cmd
		m.relocInput, cmd = m.relocInput.Update(x)
		return m, cmd
	}
}

// relocateTab is the Tab completion for the input phase. The first press extends to the
// longest common prefix; once that is exhausted it cycles through candidate directories.
func (m Model) relocateTab() Model {
	cur := m.relocInput.Value()
	// If we are cycling (the value is unchanged from the last applied candidate), advance to the next one.
	if c := m.relocCycle; c != nil && cur == c.applied {
		c.idx = (c.idx + 1) % len(c.names)
		c.applied = filepath.Join(c.target, c.names[c.idx]) + "/"
		m.relocInput.SetValue(c.applied)
		m.relocInput.CursorEnd()
		return m
	}
	completed, names := completePath(cur)
	if len(names) == 0 {
		m.relocCycle, m.relocCands = nil, nil
		return m
	}
	if len(names) == 1 {
		m.relocInput.SetValue(completed)
		m.relocInput.CursorEnd()
		m.relocCycle, m.relocCands = nil, nil
		return m
	}
	// Multiple candidates: first extend to the common prefix and show them in a grid (zsh: the first press only lists).
	if strings.TrimRight(completed, "/") != strings.TrimRight(expandUser(cur), "/") {
		m.relocInput.SetValue(completed)
		m.relocInput.CursorEnd()
		m.relocCycle = nil
		m.relocCands = names
		return m
	}
	// Common prefix already exhausted -> start menu cycling (zsh AUTO_MENU: select and insert the first candidate).
	target, _, _ := dirCandidates(cur)
	c := &relocCycle{target: target, names: names, idx: 0}
	c.applied = filepath.Join(target, names[0]) + "/"
	m.relocInput.SetValue(c.applied)
	m.relocInput.CursorEnd()
	m.relocCycle = c
	m.relocCands = names
	return m
}

// relocGridLines returns the Tab-completion candidates rendered as a zsh-style grid (the candidate being cycled is inverted).
func (m Model) relocGridLines() []string {
	if len(m.relocCands) == 0 {
		return nil
	}
	disp := make([]string, len(m.relocCands))
	for i, n := range m.relocCands {
		disp[i] = n + "/" // directory candidates are shown with a trailing "/", like zsh
	}
	rows, cellW := candidateGrid(disp, max(1, m.width-1))
	sel := -1
	if m.relocCycle != nil {
		sel = m.relocCycle.idx
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		var b strings.Builder
		for _, idx := range row {
			if idx < 0 {
				continue
			}
			b.WriteString(styled(padCol(disp[idx], cellW), "", idx == sel, false))
		}
		lines = append(lines, b.String())
	}
	return lines
}

func (m Model) View() string {
	if m.action == "confirm-fork" {
		return "Fork from selected turn? y/n"
	}
	if m.action == "relocate" {
		s := fmt.Sprintf("Relocate sessions\n\nNew path (%s →)\n\n> %s\n", m.relocOld, m.relocInput.View())
		if grid := m.relocGridLines(); len(grid) > 0 {
			s += "\n" + strings.Join(grid, "\n") + "\n"
		}
		return s + "\nTab complete  Enter confirm  Esc cancel"
	}
	if m.action == "relocate-confirm" {
		return fmt.Sprintf("Relocate %d session(s)?\n\n  %s\n  → %s\n\ny execute  n/Esc cancel",
			m.relocCount, m.relocOld, m.relocInput.Value())
	}
	if m.detail != nil {
		return m.detailView()
	}
	nlive := 0
	for _, s := range m.sessions {
		if s.Status != "" {
			nlive++
		}
	}
	head := fmt.Sprintf(" AgentCarto   view: %s   sessions: %d/%d   active: %d ", m.view, len(m.rows), len(m.sessions), nlive)
	if m.scanning {
		head += " scanning..."
	}
	if m.activeOnly {
		head += " [active]"
	}
	// Said when the column is not what it usually is. Sessions without a headline
	// keep their title either way, so a mixed list does not say for itself.
	if m.listTitles {
		head += " [title]"
	}
	var b strings.Builder
	b.WriteString(clip(head, max(20, m.width-1)) + "\n")
	// Row 1 (the search/status line) is always reserved as one full-width line. While
	// searching or filtering it holds the input field; otherwise a full-width blank line.
	// Using a real full-width line rather than an empty string keeps the start row and row
	// count from jittering as search is toggled, so rendering stays stable.
	if m.searching || m.query.Value() != "" {
		b.WriteString(m.query.View() + "\n")
	} else {
		b.WriteString(strings.Repeat(" ", max(1, m.width-1)) + "\n")
	}
	bodyRows := m.listBodyRows()
	// Recompute offset every frame so the selected row is always inside the visible
	// window. This keeps the cursor and display in sync even for state transitions that
	// forgot to call ensureListOffset (e.g. entering search). m.offset itself is settled
	// on the next operation.
	offset := m.offset
	if m.cursor < offset {
		offset = m.cursor
	}
	if m.cursor >= offset+bodyRows {
		offset = m.cursor - bodyRows + 1
	}
	offset = max(0, min(offset, max(0, len(m.rows)-bodyRows)))
	maxN := 0
	if m.index != nil {
		maxN = m.index.MaxCount()
	}
	rendered := 0
	for pos := offset; pos < len(m.rows) && pos < offset+bodyRows; pos++ {
		r := m.rows[pos]
		if r.Kind == "header" {
			b.WriteString(m.folderHeaderRow(r, pos == m.cursor) + "\n")
		} else {
			b.WriteString(m.sessionRow(m.sessions[r.SessIdx], pos == m.cursor, maxN, r.TreePrefix) + "\n")
		}
		rendered++
	}
	// When the body is shorter than the viewport, pad with blank lines so the footer is
	// always pinned to the last row. Without padding, the footer would float up on a short list.
	if m.height > 0 {
		for i := rendered; i < bodyRows; i++ {
			b.WriteString("\n")
		}
	}
	if m.flash != "" {
		b.WriteString(flashBar(padCol(clip(m.flash, max(1, m.width-1)), max(1, m.width-1))))
	} else {
		foot := " ↑↓/jk move  Enter open/toggle  o resume  c cd  m move  v view  a active"
		// Names what the key switches to, not what is on screen, and offered only
		// when there is something to switch to — on a machine that has generated
		// no summaries yet both columns would be the title.
		if len(m.headlines) > 0 {
			if m.listTitles {
				foot += "  s summary"
			} else {
				foot += "  s title"
			}
		}
		foot += "  / search  r reload  q quit "
		b.WriteString(footer(padCol(clip(foot, max(1, m.width-1)), max(1, m.width-1))))
	}
	return b.String()
}

func (m Model) detailView() string {
	// openCurrentTurn keeps turnOpen and the cursor in step, so the row is a turn
	// whenever the turn view is open. Check it here anyway: a render that finds
	// otherwise falls back to the turn list instead of recursing.
	if m.turnOpen {
		if row, ok := m.selectedDetailRow(); ok && row.Kind == "turn" {
			return m.turnFullView(row)
		}
	}
	if len(m.detailRows) == 0 && len(m.detailTurns) > 0 {
		path := m.currentDetailPath()
		if len(path) == 0 && m.detail != nil {
			path = m.detail.ActivePath()
		}
		(&m).rebuildDetailRows(path)
	}
	var b strings.Builder
	s := m.detailSession
	bodyRows := m.detailBodyRows()
	offset := m.detailOffset
	if offset > max(0, len(m.detailRows)-bodyRows) {
		offset = max(0, len(m.detailRows)-bodyRows)
	}
	if offset < 0 {
		offset = 0
	}
	end := min(len(m.detailRows), offset+bodyRows)
	colW := m.detailColWidths(s, offset, end)
	modelW := m.detailModelWidth(s, offset, end)
	b.WriteString(clip(m.detailLead(s), max(20, m.width-1)) + "\n")
	b.WriteString(m.detailCWDLine(s) + "\n")
	// turnHits is ascending over all rows; only the visible ones are rendered.
	hit := map[int]bool{}
	for _, i := range m.turnHits {
		if i >= offset && i < end {
			hit[i] = true
		}
	}
	bodyLines := []string{}
	for i := offset; i < end; i++ {
		rowData := m.detailRows[i]
		selected := i == m.detailCursor
		switch rowData.Kind {
		case "head", "headtext":
			// The rows are rebuilt whenever the width changes, so the count here
			// agrees with the lines. Should one ever not, an empty line beats
			// drawing some other row's.
			cursor := -1
			if selected {
				cursor = rowData.Line
			}
			if lines := m.detailHeadLines(s, cursor); rowData.Line < len(lines) {
				bodyLines = append(bodyLines, lines[rowData.Line])
			} else {
				bodyLines = append(bodyLines, "")
			}
		case "forkparent":
			bodyLines = append(bodyLines, m.detailForkParentLine(selected))
		case "branch":
			bodyLines = append(bodyLines, m.detailBranchLine(rowData, selected, colW, modelW))
		default:
			bodyLines = append(bodyLines, m.detailTurnLine(s, rowData, selected, colW, modelW, hit[i]))
		}
	}
	if len(bodyLines) > bodyRows {
		bodyLines = bodyLines[:bodyRows]
	}
	for _, line := range bodyLines {
		b.WriteString(line + "\n")
	}
	for i := len(bodyLines); i < bodyRows; i++ {
		b.WriteString("\n")
	}
	if m.flash != "" {
		// A flash takes priority over the normal/search footer and uses its own colors
		// (resume/fork errors, the parent-jump notice, etc.). It clears automatically after
		// flashTTL and the original footer returns.
		b.WriteString(flashBar(padCol(clip(m.flash, max(1, m.width-1)), max(1, m.width-1))))
		return b.String()
	}
	b.WriteString(footer(padCol(clip(m.detailFooter(s), max(1, m.width-1)), max(1, m.width-1))))
	return b.String()
}

// detailColWidths computes the display width of each turn-mark column across the visible
// rows so the columns line up.
func (m Model) detailColWidths(s *domain.Session, offset, end int) [9]int {
	var colW [9]int
	for i := offset; i < end; i++ {
		if m.detailRows[i].Kind != "turn" {
			continue
		}
		parts := m.turnMarkParts(m.detailRows[i].Turn, m.turnRunningNow(s, m.detailRows[i].TurnIndex))
		for j, p := range parts {
			if w := runewidth.StringWidth(p); w > colW[j] {
				colW[j] = w
			}
		}
	}
	return colW
}

// detailModelColMax caps the per-turn model column so a long model name cannot
// crowd out the headline on a narrow terminal.
const detailModelColMax = 22

// turnModel returns the model to show for a turn: the first event that names
// one, falling back to the session-level model when no event carries one (the
// fallback the plugins document on Event.Model).
func turnModel(events []domain.Event, s *domain.Session) string {
	for _, e := range events {
		if e.Model != "" {
			return e.Model
		}
	}
	return s.Model
}

// detailModelWidth returns the display width of the per-turn model column across
// the visible turn rows (capped) so the column lines up.
func (m Model) detailModelWidth(s *domain.Session, offset, end int) int {
	w := 0
	for i := offset; i < end; i++ {
		if m.detailRows[i].Kind != "turn" {
			continue
		}
		if mw := runewidth.StringWidth(turnModel(m.turnEvents(m.detailRows[i].Turn), s)); mw > w {
			w = mw
		}
	}
	return min(w, detailModelColMax)
}

// detailLead builds the first header line: status mark, agent/session, the
// started→updated span, and the turn and branch counts. The model is not shown
// here; it is rendered per turn (see detailTurnLine) because it can differ
// between turns.
func (m Model) detailLead(s *domain.Session) string {
	lead := " "
	if mk := strings.TrimSpace(statusMark(*s)); mk != "" {
		lead += statusStyled(mk+" ", *s, false, true)
	}
	lead += styled(s.AgentType+"  ", m.pluginColor(*s), false, true) + styled(shortID(s.SessionID)+"   ", lipgloss.Color("3"), false, false)
	lead += s.StartedAt.Local().Format("2006-01-02 15:04") + " → " + s.UpdatedAt.Local().Format("2006-01-02 15:04") + "   "
	lead += fmt.Sprintf("turns:%d", len(m.detailTurns))
	nbranch := 0
	for _, r := range m.detailRows {
		if r.Kind == "branch" {
			nbranch++
		}
	}
	if nbranch > 0 {
		lead += fmt.Sprintf("  branches:%d", nbranch)
	}
	return lead
}

// detailCWDLine builds the second header line: the working directory on a line of its
// own. It shares the line with nothing else so a deep path is readable in full; only a
// terminal narrower than the path itself makes shortCWD shorten it.
func (m Model) detailCWDLine(s *domain.Session) string {
	w := max(20, m.width-2)
	return styled(clip(" "+shortCWD(s.CWD, w), w+1), lipgloss.Color("3"), false, false)
}

// detailSubLine builds the third header line: head — the title, or the session's
// own summary in its place (see detailHeadLines) — an optional "forked from"
// lineage, and (when descended without a fork route) a breadcrumb of the current
// level. The working directory is on its own line above (detailCWDLine).
//
// generated says the line is generated text rather than something the session
// recorded, which is the difference between the two things head can be. It picks
// the color the rest of the program gives generated text; that this currently
// resolves to the same color as a title is why the § mark, not the color, is
// what tells them apart.
// selected marks it as the cursor's row: it is a row of the list, not a header.
func (m Model) detailSubLine(s *domain.Session, head string, generated, selected bool) string {
	sub := " " + head
	if s.LogDeleted {
		// Said here as well as in the list: this is the screen someone reads before
		// trying to resume, and the log it would resume from is not there.
		sub = " ⌫ log deleted (read from the cache) ·" + sub
	}
	// Use the same "forked from: <lineage>" wording whether a fork was opened directly or descended into.
	route := m.detailForkRoute()
	if len(route) > 0 {
		sub += "   forked from: " + strings.Join(route, " › ")
		// The same condition that puts the row under turn #1 there, so the
		// heading, the row and the key all agree on whether the parent is
		// one move away.
		if m.forkParentRowVisible() {
			sub += " (p)"
		}
	}
	color := lipgloss.Color("3")
	if generated {
		color = roleColor("meta")
	}
	w := max(20, m.width-1)
	sub = clip(sub, w)
	// When descended without a fork route shown (e.g. rewind), use a breadcrumb to
	// indicate which level we are on. It is measured before the padding below,
	// because padding to the full width first would leave it no room at all.
	crumb := ""
	if len(m.detailPathStack) > 1 && len(route) == 0 {
		if avail := max(0, w-runewidth.StringWidth(sub)); avail > 0 {
			crumb = clip("  ▸ "+m.detailCrumb(), avail)
		}
	}
	if selected {
		// Selected rows are padded to the width so the highlight covers the line,
		// as the turn rows are — less whatever the breadcrumb takes.
		sub = padCol(sub, max(0, w-runewidth.StringWidth(crumb)))
	}
	subLine := styled(sub, color, selected, false)
	if crumb != "" {
		subLine += styled(crumb, lipgloss.Color("5"), true, false)
	}
	return subLine
}

// summaryMark prefixes every line an agent wrote rather than the session. One
// character rather than a heading of its own: the head is one row when it is
// closed, and the mark is the part of it that has to be there.
const summaryMark = "§"

// detailHeadLines renders the header lines under the working directory, which
// hold either the session's own generated summary (stored as turn 0) or the
// title.
//
// Closed, that is one line: the session's headline when there is one, the title
// when there is not. The title is the session's first prompt, which turn #1
// carries as its own headline — with a summary to show, spending the line on the
// title says what the screen says anyway, and the prompt itself is one row away.
// Open (`s`), that line stays and the whole summary follows it.
//
// These lines are rows of the list rather than part of the fixed header, so an
// opened summary scrolls with everything else and is never cut to fit: what the
// header cannot hold, the cursor reaches. rebuildDetailRows counts them and
// detailView draws them, both through here.
//
// cursor is which of the returned lines the cursor is on, or -1 for none: every
// one of them is a row the cursor can reach, so every one of them has to be able
// to show that it is the row.
func (m Model) detailHeadLines(s *domain.Session, cursor int) []string {
	if s == nil {
		return nil
	}
	// A blank turn-0 row is the store's note that the session was looked at, not
	// a summary of it (cache.MarkExamined), and reads here as having none.
	sum := strings.TrimSpace(m.summaries[0].Text)
	if sum == "" {
		return []string{m.detailSubLine(s, s.Title, false, cursor == 0)}
	}
	// Closed, the head is one line. That is what the model wrote it for — the
	// headline says what the session was in a line, where the summary's first
	// line is only its first line. A summary written before headlines existed has
	// none, and then the paragraph is flattened into the line as before.
	one := strings.TrimSpace(m.summaries[0].Headline)
	if one == "" {
		// Flattened rather than cut at the first line break: the line is clipped to
		// the width anyway, and a summary that opens with a short line would
		// otherwise show a short line where the whole width was available.
		one = strings.Join(strings.Fields(sum), " ")
	}
	if m.summaryStale {
		// Said before the summary rather than after it, because after it is what a
		// narrow line drops. The turns below are current; only this is not.
		sum, one = "(stale) "+sum, "(stale) "+one
	}
	w := max(20, m.width-1)
	if !m.summaryOpen {
		return []string{m.detailSubLine(s, summaryMark+" "+one, true, cursor == 0)}
	}
	// Open, the summary unfolds under a first line rather than replacing it. What
	// that line gave up — the session's first prompt — is turn #1's headline, one
	// row down the list, so it does not come back here.
	body := []string{}
	for _, para := range strings.Split(sum, "\n") {
		body = append(body, wrapWidth(strings.TrimSpace(para), w-3)...)
	}
	lead := one
	if strings.TrimSpace(m.summaries[0].Headline) == "" && len(body) > 0 {
		// Without a headline the closed line is the paragraph flattened, so
		// repeating it above the paragraph would say the same thing twice. The
		// paragraph's own first line leads instead.
		lead, body = body[0], body[1:]
	}
	out := []string{m.detailSubLine(s, summaryMark+" "+lead, true, cursor == 0)}
	for i, ln := range body {
		body[i] = "  " + ln
	}
	// Who wrote it and when, so a reader can weigh it: a summary from a smaller
	// model reads differently, and one written days ago describes the session as
	// it was then. It trails the paragraph when the last line has room for it.
	if credit := m.summaryCredit(); credit != "" {
		if last := len(body) - 1; last >= 0 && runewidth.StringWidth(body[last])+runewidth.StringWidth(credit)+2 <= w {
			body[last] += "  " + credit
		} else {
			body = append(body, "  "+credit)
		}
	}
	for _, ln := range body {
		out = append(out, headLine(ln, roleColor("meta"), w, cursor == len(out)))
	}
	return out
}

// headLine renders one line of the head, padded to the width when it is the
// cursor's row so the highlight covers the whole line, as the turn rows are.
func headLine(text string, color lipgloss.Color, w int, selected bool) string {
	if selected {
		text = padCol(clip(text, w), w)
	}
	return styled(clip(text, w), color, selected, false)
}

// hasSessionSummary reports whether the head row holds a generated summary,
// which is what makes it worth opening (with no summary it is the title, and
// there is nothing under it).
func (m Model) hasSessionSummary() bool { return strings.TrimSpace(m.summaries[0].Text) != "" }

// summaryGrace is how far behind the turns a session summary may fall before the
// screen says so, which is the interval it is remade on: a turn described since
// the last one is the pacing working, not the summary going out of date. A model
// built without a config — a test — asks the strict question instead.
func (m Model) summaryGrace() time.Duration {
	if m.app == nil {
		return 0
	}
	return time.Duration(m.app.Config.Summary.SessionInterval)
}

// summaryCredit names what wrote the session summary and when it was written,
// or "" when neither is known. The time comes from the store rather than the
// summary text, which is the agent's answer and carries no time of its own.
func (m Model) summaryCredit() string {
	parts := []string{}
	// The session summary's own model: a turn's can come from a different run, and
	// naming that one here would credit the paragraph to something that did not
	// write it.
	if model := m.summaries[0].Model; model != "" {
		parts = append(parts, model)
	}
	if !m.summaryAt.IsZero() {
		parts = append(parts, m.summaryAt.Local().Format("2006-01-02 15:04"))
	}
	if len(parts) == 0 {
		return ""
	}
	return "— " + strings.Join(parts, " · ")
}

// detailBranchLine renders a branch row: an alternative sub-tree (fork or rewind) the
// cursor can descend into, with its turn/message/branch counts and lead text.
func (m Model) detailBranchLine(rowData detailRow, selected bool, colW [9]int, modelW int) string {
	conn := "├─"
	if rowData.LastBranch {
		conn = "└─"
	}
	bt := transcript.Turns(*m.detail, convlogic.DeepestPath(*m.detail, rowData.Root))
	sz := len(convlogic.Subtree(*m.detail, rowData.Root))
	nb := convlogic.BranchAltCount(*m.detail, rowData.Root)
	indent := strings.Repeat(" ", m.detailTurnHeadlineColumn(rowData, colW, modelW))
	label := fmt.Sprintf("%s%s %s (%dturn/%dmsg/%dbranch) %s", indent, conn, convlogic.BranchKind(*m.detail, rowData.Root), len(bt), sz, nb, convlogic.BranchLead(*m.detail, rowData.Root))
	line := styled(clip(label, max(20, m.width-1)), lipgloss.Color("5"), selected, false)
	if selected && lipgloss.Width(line) < m.width-1 {
		line += styled(strings.Repeat(" ", m.width-1-lipgloss.Width(line)), "", true, false)
	}
	return line
}

// forkParentRowVisible reports whether the turn list should offer the row that
// leads to the session this fork was made from.
//
// Opening a fork directly focuses a frame on its own line automatically, and
// that frame is still the session the reader opened — the row belongs there.
// Frames entered by hand are a different matter: the list is then showing some
// other line of conversation, and a row about this session's origin would be
// answering a question nobody asked.
func (m Model) forkParentRowVisible() bool {
	if m.detailSession == nil || m.sessionParentID(*m.detailSession) == "" {
		return false
	}
	for i := 1; i < len(m.detailPathStack); i++ { // 0 is the base frame, never entered by hand
		if !m.detailPathStack[i].followFocus {
			return false
		}
	}
	return true
}

// forkParentLabel is the undecorated text of the fork-parent row, shared by the
// row itself and by what a turn search matches against.
func (m Model) forkParentLabel() string {
	if m.detailSession == nil {
		return ""
	}
	pid := m.sessionParentID(*m.detailSession)
	label := "↳" + shortID(pid) + " forked from"
	p := m.findSession(m.detailSession.PluginID, pid)
	if p == nil {
		return label + " (not loaded)"
	}
	if t := strings.Join(strings.Fields(p.Title), " "); t != "" {
		label += " " + t
	}
	return label
}

// detailForkParentLine renders the row under turn #1 that leads to the session
// this fork was made from. It names the parent by the same ↳<id> the session
// list uses, and adds its title so the row says which conversation it opens. A
// parent that is not in the list is still named — knowing a fork came from
// somewhere is worth more than hiding the row — and selecting it explains
// itself through jumpToParent's message.
func (m Model) detailForkParentLine(selected bool) string {
	line := styled(clip("    "+m.forkParentLabel(), max(20, m.width-1)), lipgloss.Color("5"), selected, false)
	if selected && lipgloss.Width(line) < m.width-1 {
		line += styled(strings.Repeat(" ", m.width-1-lipgloss.Width(line)), "", true, false)
	}
	return line
}

// detailTurnLine renders a turn row: the leading marker, turn number, timestamp, the
// turn-mark columns (reply/tool/edit counts), and the headline.
func (m Model) detailTurnLine(s *domain.Session, rowData detailRow, selected bool, colW [9]int, modelW int, hit bool) string {
	turn := rowData.Turn
	chronIndex := rowData.TurnIndex
	events := m.turnEvents(turn)
	var ts time.Time
	for _, e := range events {
		if ts.IsZero() && !e.Timestamp.IsZero() {
			ts = e.Timestamp
		}
	}
	// Leading marker (fixed width 2): ● for the newest in-progress turn, » for a /compact boundary, blank otherwise.
	mark, markRole := "  ", ""
	if s.Status == domain.StatusRunning && chronIndex == m.detailNewestChron {
		mark, markRole = "● ", "running"
	} else if rowData.Badge {
		mark, markRole = "» ", "badge"
	}
	parts := m.turnMarkParts(turn, m.turnRunningNow(s, chronIndex))
	var row strings.Builder
	row.WriteString(styled(mark, roleColor(markRole), selected, false))
	row.WriteString(styled(fmt.Sprintf("#%-4d ", chronIndex+1), "", selected, false))
	row.WriteString(styled(func() string {
		if ts.IsZero() {
			return "-----"
		}
		return ts.Local().Format("01-02 15:04")
	}()+"  ", "", selected, false))
	roles := [9]string{"user", "user", "assistant", "tool", "task", "badge", "user", "add", "del"}
	for j, part := range parts {
		if colW[j] == 0 {
			continue
		}
		cell := alignMark(part, colW[j])
		content := cell + " "
		// Only the elapsed-time column (the first one) of the selected in-progress turn
		// gets a lit background during the on phase. The off phase falls through to the
		// styled call below and returns to the selection background (same as the whole
		// row), so the background color blinks. The background is applied only to the
		// cell body; the separator space to its right never blinks and always uses the
		// row background.
		if j == 0 && markRole == "running" && selected && m.blinkOn {
			row.WriteString(lipgloss.NewStyle().
				Foreground(lipgloss.Color("0")).
				Background(roleColor("running")).
				Render(cell))
			row.WriteString(styled(" ", roleColor(roles[j]), selected, false))
			continue
		}
		row.WriteString(styled(content, roleColor(roles[j]), selected, false))
	}
	// Per-turn model column, replacing the session-level model that used to sit
	// in the header. Placed after the time/count columns (so the start time and
	// elapsed-time columns stay adjacent) and before the headline; fixed width
	// (padCol) so it lines up across rows.
	if modelW > 0 {
		row.WriteString(styled(padCol(turnModel(events, s), modelW)+"  ", lipgloss.Color("3"), selected, false))
	}
	remain := max(1, m.width-1-lipgloss.Width(row.String()))
	headColor := lipgloss.Color("")
	if !selected && hit {
		headColor = roleColor("meta") // emphasize the headline of a search hit
	}
	row.WriteString(styled(clip(convlogic.TurnHeadline(*m.detail, turn), remain), headColor, selected, false))
	line := row.String()
	if selected && lipgloss.Width(line) < m.width-1 {
		line += styled(strings.Repeat(" ", m.width-1-lipgloss.Width(line)), "", true, false)
	}
	return line
}

// detailTurnHeadlineColumn returns the visual column where this turn's headline
// starts. Branch rows use the same column, mirroring how session-tree connectors
// start at the title column instead of shifting the fixed metadata columns.
func (m Model) detailTurnHeadlineColumn(rowData detailRow, colW [9]int, modelW int) int {
	events := m.turnEvents(rowData.Turn)
	stamp := "-----"
	for _, e := range events {
		if !e.Timestamp.IsZero() {
			stamp = e.Timestamp.Local().Format("01-02 15:04")
			break
		}
	}
	w := 2 + runewidth.StringWidth(fmt.Sprintf("#%-4d ", rowData.TurnIndex+1)) + runewidth.StringWidth(stamp+"  ")
	for _, width := range colW {
		if width > 0 {
			w += width + 1
		}
	}
	if modelW > 0 {
		w += modelW + 2
	}
	return w
}

// detailFooter returns the detail-view footer: the search footer while searching,
// otherwise the key hints (with "p parent" only when the session has a parent).
func (m Model) detailFooter(s *domain.Session) string {
	if m.turnSearching || m.turnQuery != "" {
		return m.searchFooter(" Search > "+m.turnQuery, len(m.turnListHits()), m.turnSearchPos)
	}
	foot := " ↑↓/jk move  Enter open  y copy  x export  o resume  c cd  / search  f fork"
	// Offered only when it would do something, so the key is never a promise the
	// screen cannot keep.
	if m.hasSessionSummary() {
		foot += "  s summary"
	}
	if m.sessionParentID(*s) != "" {
		foot += "  p parent"
	}
	foot += "  q/← back "
	return foot
}

// searchFooter composes the search prompt with the hit count and current position into one line.
func (m Model) searchFooter(prompt string, nhits, pos int) string {
	cnt := "no match"
	if nhits > 0 {
		if pos < 0 {
			cnt = fmt.Sprintf("%d hits", nhits)
		} else {
			cnt = fmt.Sprintf("%d/%d", pos+1, nhits)
		}
	}
	return fmt.Sprintf("%s    %s   Enter jump  n/N next/prev  Esc clear ", prompt, cnt)
}
func (m *Model) ensureDetailOffset() {
	rows := m.detailBodyRows()
	if m.detailCursor < m.detailOffset {
		m.detailOffset = m.detailCursor
	}
	if m.detailCursor >= m.detailOffset+rows {
		m.detailOffset = m.detailCursor - rows + 1
	}
	m.detailOffset = max(0, min(m.detailOffset, max(0, len(m.detailRows)-rows)))
}

func (m Model) currentDetailPath() []string {
	if len(m.detailPathStack) > 0 {
		return m.detailPathStack[len(m.detailPathStack)-1].path
	}
	if m.detail != nil {
		return m.detail.ActivePath()
	}
	return nil
}

// branchRootOf returns the root of the branch containing leaf (the first node that
// diverges from the active path). If leaf is on the active path (the main backbone) it
// returns "" (no descent needed). In the root-anchored view it is used to derive a
// fork's branch root from its active leaf.
func branchRootOf(c domain.Conversation, leaf string) string {
	active := map[string]bool{}
	for _, id := range c.ActivePath() {
		active[id] = true
	}
	if leaf == "" || active[leaf] {
		return ""
	}
	cur := leaf
	for {
		n, ok := c.Nodes[cur]
		if !ok || n.Parent == "" || active[n.Parent] {
			return cur // parent is on the backbone (or there is no parent): cur is the branch root
		}
		cur = n.Parent
	}
}

// directFocusFrame builds the automatically followed frame used when a fork
// session is opened directly. Its path ends at focusLeaf even when the branch
// root also contains a newer child fork.
func (m Model) directFocusFrame(c domain.Conversation, focusLeaf string) (detailFrame, bool) {
	root := branchRootOf(c, focusLeaf)
	if root == "" {
		return detailFrame{}, false
	}
	path := pathFromRootToLeaf(c, root, focusLeaf)
	if len(path) == 0 {
		return detailFrame{}, false
	}
	return detailFrame{path: path, label: m.branchFrameLabel(root), followFocus: true}, true
}

// pathFromRootToLeaf returns the unique parent path from root through leaf. A
// missing node, a parent cycle, or a root that is not an ancestor returns nil.
func pathFromRootToLeaf(c domain.Conversation, root, leaf string) []string {
	if root == "" || leaf == "" {
		return nil
	}
	reversed := make([]string, 0, 8)
	seen := map[string]bool{}
	for cur := leaf; cur != "" && !seen[cur]; {
		seen[cur] = true
		node, ok := c.Nodes[cur]
		if !ok {
			return nil
		}
		reversed = append(reversed, cur)
		if cur == root {
			for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
				reversed[i], reversed[j] = reversed[j], reversed[i]
			}
			return reversed
		}
		cur = node.Parent
	}
	return nil
}

// findSession looks up the (plugin, sessionID) session in the list (used to walk a fork lineage).
func (m Model) findSession(pluginID, sessionID string) *domain.Session {
	for i := range m.sessions {
		if m.sessions[i].PluginID == pluginID && m.sessions[i].SessionID == sessionID {
			return &m.sessions[i]
		}
	}
	return nil
}

// sessionParentID is the single source of truth for user-facing hierarchy and
// parent navigation. ParentSessionID remains the recorded creation provenance;
// once a scan has resolved the canonical conversation parent, every UI surface
// uses that logical relationship instead.
func (m Model) sessionParentID(s domain.Session) string {
	if pid, ok := m.sessionParents[s.Key()]; ok {
		return pid
	}
	return s.ParentSessionID
}

// forkLineage returns the short IDs of fork session s's ancestors (following
// the canonical conversation parent: root → … → immediate parent). It stops once
// a parent is no longer in the list (still emitting that ID).
func (m Model) forkLineage(s domain.Session) []string {
	var anc []string // collected as immediate parent → … → root
	seen := map[string]bool{s.SessionID: true}
	pid := m.sessionParentID(s)
	for pid != "" && !seen[pid] {
		seen[pid] = true
		anc = append(anc, shortID(pid))
		p := m.findSession(s.PluginID, pid)
		if p == nil {
			break
		}
		pid = m.sessionParentID(*p)
	}
	for i, j := 0, len(anc)-1; i < j; i, j = i+1, j-1 { // reverse to root → immediate parent order
		anc[i], anc[j] = anc[j], anc[i]
	}
	return anc
}

// detailForkRoute returns the lineage (root → …) shown after "forked from" when the
// current detail view is fork content. When a fork session is opened directly, this is
// that session's ancestors; when descended into a fork branch, the fork's parent is the
// current session, so it returns the current session's lineage plus the current session
// ID. For non-forks (rewind descent, ordinary sessions) it returns nil.
//
// Whether the parent is one move away is a separate question, answered by
// forkParentRowVisible: a directly opened fork focuses a frame of its own, which
// this function reads as descent but the reader does not.
func (m Model) detailForkRoute() []string {
	s := m.detailSession
	if s == nil {
		return nil
	}
	if len(m.detailPathStack) > 1 {
		last := m.detailPathStack[len(m.detailPathStack)-1]
		if m.detail != nil && len(last.path) > 0 && convlogic.BranchKind(*m.detail, last.path[0]) == "fork" {
			return append(m.forkLineage(*s), shortID(s.SessionID))
		}
		return nil // descent into a rewind uses the normal breadcrumb
	}
	if m.sessionParentID(*s) != "" {
		return m.forkLineage(*s) // a fork was opened directly
	}
	return nil
}

// branchFrameLabel is the breadcrumb label for a frame descended into another lineage.
// It combines the branch kind and the branch root's short ID ("kind shortID") so the
// branch can be told apart.
func (m Model) branchFrameLabel(root string) string {
	if m.detail == nil {
		return shortID(root)
	}
	return convlogic.BranchKind(*m.detail, root) + " " + shortID(root)
}

// forkBranchSession resolves a grafted fork row to the child session that owns
// its root. Rewinds and same-session branches intentionally keep the existing
// in-place path drilldown behavior.
func (m Model) forkBranchSession(root string) (domain.Session, bool) {
	if m.detail == nil || m.detailSession == nil || convlogic.BranchKind(*m.detail, root) != "fork" {
		return domain.Session{}, false
	}
	origin, ok := m.detailOrigins[root]
	if !ok || origin.Session.Key() == m.detailSession.Key() {
		return domain.Session{}, false
	}
	if current := m.findSession(origin.Session.PluginID, origin.Session.SessionID); current != nil {
		return *current, true
	}
	return origin.Session, true
}

// detailCrumb is the breadcrumb of the current branch ("current › fork 1a2b3c4d › …"), used to tell descended frames and forks apart.
func (m Model) detailCrumb() string {
	parts := make([]string, len(m.detailPathStack))
	for i, f := range m.detailPathStack {
		parts[i] = f.label
	}
	return strings.Join(parts, " › ")
}

func (m *Model) ensureDetailRowsBuilt() {
	if len(m.detailRows) > 0 || len(m.detailTurns) == 0 {
		return
	}
	path := m.currentDetailPath()
	if len(path) == 0 && m.detail != nil {
		path = m.detail.ActivePath()
	}
	m.rebuildDetailRows(path)
}

func (m *Model) setDetailPath(path []string) {
	m.rebuildDetailRows(path)
}

// rebuildDetailRows builds the displayed turns (detailTurns) and rows (detailRows)
// from path, newest first. Which turns are shown, their numbering and their »
// badge come from transcript.Turns, so the list, an export and the CLI all agree
// on what "turn #N" is; the rows add the branch entries, which are the TUI's own.
func (m *Model) rebuildDetailRows(path []string) {
	m.detailTurns = nil
	m.detailRows = nil
	m.detailNewestChron = -1
	m.turnHitsStale = true
	m.detailActive = nil
	if m.detail == nil {
		return
	}
	active := map[string]bool{}
	for _, id := range path {
		active[id] = true
	}
	// Branch rows and per-turn rewind marks must agree on which continuation is
	// being read. In a focused fork frame that is path, not the synthesized
	// conversation's parent-session ActivePath.
	m.detailActive = active
	// The head rows first: the session's summary (or its title) is what the
	// session amounted to, and the turns below it are how it got there. Rows
	// rather than a header line, so an opened summary scrolls instead of being
	// cut to whatever the header could spare.
	for i := range m.detailHeadLines(m.detailSession, -1) {
		kind := "headtext"
		if i == 0 {
			kind = "head"
		}
		m.detailRows = append(m.detailRows, detailRow{Kind: kind, Line: i})
	}
	turns := transcript.Turns(*m.detail, path)
	for i := len(turns) - 1; i >= 0; i-- { // newest first
		e := turns[i]
		if e.Index > m.detailNewestChron {
			m.detailNewestChron = e.Index
		}
		m.detailTurns = append(m.detailTurns, e.Nodes)
		m.detailRows = append(m.detailRows, detailRow{Kind: "turn", Turn: e.Nodes, TurnIndex: e.Index, Badge: e.Compact})
		_, subs := convlogic.TurnBranches(*m.detail, e.Nodes, active)
		for j, root := range subs {
			m.detailRows = append(m.detailRows, detailRow{Kind: "branch", Turn: e.Nodes, TurnIndex: e.Index, Root: root, LastBranch: j == len(subs)-1})
		}
	}
	// A fork carries a copy of what was said before it, so its oldest turn is
	// where the parent session continues. Rows run newest first, so the row that
	// leads there belongs at the very bottom, under turn #1.
	if m.forkParentRowVisible() {
		m.detailRows = append(m.detailRows, detailRow{Kind: "forkparent"})
	}
}

func dropLastRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}

// detailRowText returns the lowercased text used for search matching (turn =
// headline + body, branch = lead, fork parent = the parent's id and title).
func (m Model) detailRowText(r detailRow) string {
	if m.detail == nil {
		return ""
	}
	if r.Kind == "headtext" {
		// The lines an open summary wraps into are not hits of their own: it is
		// matched once, on the head row above them.
		return ""
	}
	if r.Kind == "head" {
		// The generated summary is searchable here because it is searchable at the
		// list level (cache.SummaryTexts): a session found by its summary and then
		// opened has to find it inside as well. The headline is matched too — it is
		// what the row shows while it is closed, and a word on screen that finds
		// nothing reads as a broken search. The title is not matched: it is turn
		// #1's headline, and matching both would report one hit twice.
		return strings.ToLower(m.summaries[0].Headline + " " + m.summaries[0].Text)
	}
	if r.Kind == "forkparent" {
		return strings.ToLower(m.forkParentLabel())
	}
	if r.Kind == "branch" {
		return strings.ToLower(convlogic.BranchLead(*m.detail, r.Root))
	}
	var b strings.Builder
	b.WriteString(convlogic.TurnHeadline(*m.detail, r.Turn))
	for _, e := range m.turnEvents(r.Turn) {
		b.WriteByte(' ')
		b.WriteString(e.Text)
		if e.ToolName != "" {
			b.WriteByte(' ')
			b.WriteString(e.ToolName)
		}
		// The one-line argument is what the list-level search matches on (a command,
		// a path). Leaving it out here would find a session by its tool call and
		// then find nothing inside it.
		if e.ToolArg != "" {
			b.WriteByte(' ')
			b.WriteString(e.ToolArg)
		}
	}
	return strings.ToLower(b.String())
}

// turnListHits returns the indices of detailRows matching the query (in display order, newest first).
func (m Model) turnListHits() []int { return m.turnHits }

// syncTurnHits recomputes turnListHits when the query changed or the rows its
// indices point into were rebuilt. It is a no-op otherwise, so the common update
// (a cursor move, the blink tick) costs one comparison.
func (m *Model) syncTurnHits() {
	if !m.turnHitsStale && m.turnQuery == m.turnHitsQuery {
		return
	}
	m.turnHitsStale = false
	m.turnHitsQuery = m.turnQuery
	m.turnHits = nil
	if strings.TrimSpace(m.turnQuery) == "" {
		return
	}
	q := strings.ToLower(m.turnQuery)
	for i, r := range m.detailRows {
		if strings.Contains(m.detailRowText(r), q) {
			m.turnHits = append(m.turnHits, i)
		}
	}
}

func (m *Model) jumpTurnHit(forward bool) {
	hits := m.turnListHits()
	if len(hits) == 0 {
		return
	}
	switch {
	case m.turnSearchPos < 0:
		m.turnSearchPos = 0
	case forward:
		m.turnSearchPos = (m.turnSearchPos + 1) % len(hits)
	default:
		m.turnSearchPos = (m.turnSearchPos - 1 + len(hits)) % len(hits)
	}
	m.detailCursor = hits[m.turnSearchPos]
	m.ensureDetailOffset()
}

// turnFullHits returns the indices of lines matching the query. It takes the
// lines rather than building them so a render, which already has them, does not
// lay out the whole turn a second time.
func (m Model) turnFullHits(lines []turnLine) []int {
	if strings.TrimSpace(m.turnFullQuery) == "" {
		return nil
	}
	q := strings.ToLower(m.turnFullQuery)
	var hits []int
	for i, ln := range lines {
		if strings.Contains(strings.ToLower(ln.text), q) {
			hits = append(hits, i)
		}
	}
	return hits
}

// applyTurnFullSearch expands any block that contains a hit, so hits inside collapsed blocks become visible.
func (m *Model) applyTurnFullSearch() {
	if strings.TrimSpace(m.turnFullQuery) == "" {
		return
	}
	q := strings.ToLower(m.turnFullQuery)
	for i, blk := range m.turnBlocks {
		if m.turnExpanded[i] || len(blk.Body) == 0 {
			continue
		}
		match := strings.Contains(strings.ToLower(blk.Label), q)
		for _, ln := range blk.Body {
			if strings.Contains(strings.ToLower(ln), q) {
				match = true
				break
			}
		}
		if match {
			m.turnExpanded[i] = true
		}
	}
}

func (m *Model) jumpTurnFullHit(forward bool) {
	hits := m.turnFullHits(m.turnFullLines())
	if len(hits) == 0 {
		return
	}
	pos := -1
	for i, h := range hits {
		if h == m.turnCursor {
			pos = i
			break
		}
	}
	var target int
	switch {
	case pos >= 0 && forward:
		target = hits[(pos+1)%len(hits)]
	case pos >= 0:
		target = hits[(pos-1+len(hits))%len(hits)]
	case forward:
		target = hits[0]
		for _, h := range hits {
			if h > m.turnCursor {
				target = h
				break
			}
		}
	default:
		target = hits[len(hits)-1]
		for i := len(hits) - 1; i >= 0; i-- {
			if hits[i] < m.turnCursor {
				target = hits[i]
				break
			}
		}
	}
	m.turnCursor = target
	m.ensureTurnOffset()
}

// turnBlockHeaderLine returns the turnFullLines index of block's header line.
func (m Model) turnBlockHeaderLine(block int) int {
	for i, ln := range m.turnFullLines() {
		if ln.header && ln.block == block {
			return i
		}
	}
	return -1
}

func (m *Model) jumpTurnBlock(delta int) {
	if len(m.turnBlocks) == 0 {
		return
	}
	cur := m.blockAtCursor()
	if cur < 0 {
		cur = 0
	}
	n := max(0, min(len(m.turnBlocks)-1, cur+delta))
	if line := m.turnBlockHeaderLine(n); line >= 0 {
		m.turnCursor = line
		m.ensureTurnOffset()
	}
}

func (m Model) selectedDetailRow() (detailRow, bool) {
	if m.detailCursor < 0 || m.detailCursor >= len(m.detailRows) {
		return detailRow{}, false
	}
	return m.detailRows[m.detailCursor], true
}

func (m *Model) openCurrentTurn(reset bool) {
	m.ensureDetailRowsBuilt()
	row, ok := m.selectedDetailRow()
	if m.detail == nil || !ok || row.Kind != "turn" {
		// Also reached from a reload (reset=false) that rebuilt the rows under an
		// already-open turn and left the cursor off a turn row. There is no turn to
		// render, so close the view rather than leave turnOpen pointing at nothing.
		m.turnOpen = false
		return
	}
	m.turnBlocks = m.turnBlocksOf(row.Turn)
	if m.turnExpanded == nil || reset {
		m.turnExpanded = map[int]bool{}
		for i, b := range m.turnBlocks {
			if b.Open {
				m.turnExpanded[i] = true
			}
		}
	}
	if reset {
		m.turnCursor = 0
		m.turnOffset = 0
		m.turnFullSearching = false
		// Carry the turn-list search query over to the full turn view, expand the blocks
		// that contain hits, and jump to the first hit.
		m.turnFullQuery = strings.TrimSpace(m.turnQuery)
		if m.turnFullQuery != "" {
			m.applyTurnFullSearch()
		}
	}
	m.turnOpen = true
	if m.turnCursor >= len(m.turnFullLines()) {
		m.turnCursor = max(0, len(m.turnFullLines())-1)
	}
	if reset && m.turnFullQuery != "" {
		m.jumpTurnFullHit(true)
	}
	m.ensureTurnOffset()
}

// turnNumberOf returns the number the detail view shows for a turn, given its
// node ids, or 0 when the turn is not on the branch being displayed. Summaries
// are keyed by that number, and a block does not carry it.
func (m Model) turnNumberOf(ids []string) int {
	if len(ids) == 0 {
		return 0
	}
	last := ids[len(ids)-1]
	for _, r := range m.detailRows {
		if r.Kind == "turn" && len(r.Turn) > 0 && r.Turn[len(r.Turn)-1] == last {
			return r.TurnIndex + 1
		}
	}
	return 0
}

func (m Model) turnBlocksOf(ids []string) []turnBlock {
	events := m.turnEvents(ids)
	out := []turnBlock{}
	// The generated summary of this turn, first and already open: it is what the
	// turn amounted to, and someone who opened a turn is deciding how much of it
	// to read. It says it is generated because everything else in this view is
	// the session verbatim, and the two must not be read as the same thing.
	if stored := m.summaries[m.turnNumberOf(ids)]; strings.TrimSpace(stored.Text) != "" {
		sum := strings.TrimSpace(stored.Text)
		label := "SUMMARY"
		if stored.Model != "" {
			label += " (generated by " + stored.Model + ")"
		}
		out = append(out, turnBlock{Sym: "§", Style: "meta", Label: label, Body: strings.Split(sum, "\n"), Open: true, NoGutter: true})
	}
	// Consolidated "edited files" section at the top of the turn: one header line
	// plus one block per file, labeled "<op> <path>  (+a -r)" with the file's
	// apply_patch hunks as Body (the "*** ... File:" header would repeat the
	// label's op/path, so it is not rendered).
	if fes := transcript.FileEdits(events); len(fes) > 0 {
		cwd := ""
		if m.detailSession != nil {
			cwd = m.detailSession.CWD
		}
		out = append(out, turnBlock{Sym: "*", Style: "tool", Label: fmt.Sprintf("Edited files (%d)", len(fes)), NoGutter: true})
		for _, fe := range fes {
			// The op/path segment is colored by op (A=green, M=yellow, D=red);
			// the "diff-" prefix keeps per-line diff styling for the body.
			style := "diff-mod"
			switch fe.Status() {
			case "A":
				style = "diff-add"
			case "D":
				style = "diff-del"
			}
			spans := []labelSpan{{fmt.Sprintf("  %s %s", fe.Status(), shortCWD(transcript.RelCWD(fe.Path, cwd), 80)), style}}
			if !fe.NoBody || fe.Added != 0 || fe.Removed != 0 {
				spans = append(spans,
					labelSpan{"  (", "plain"},
					labelSpan{fmt.Sprintf("+%d ", fe.Added), "add"},
					labelSpan{fmt.Sprintf("-%d", fe.Removed), "del"},
					labelSpan{")", "plain"},
				)
			}
			label := ""
			for _, sp := range spans {
				label += sp.text
			}
			out = append(out, turnBlock{Sym: "*", Style: style, Label: label, LabelSpans: spans, Body: fe.Body(), NoGutter: true, Path: absCWD(fe.Path, cwd)})
		}
	}
	for _, e := range events {
		if e.Kind == domain.EventMeta || transcript.InFileSection(e) {
			continue
		}
		b := eventBlock(e)
		b.Time = e.Timestamp
		out = append(out, b)
	}
	return out
}
func eventBlock(e domain.Event) turnBlock {
	text := e.Text
	lines := strings.Split(text, "\n")
	one := oneLine(text)
	switch e.Kind {
	case domain.EventUser:
		if e.Prompt == "" {
			return turnBlock{Sym: "#", Style: "meta", Label: "system: " + one, Body: lines}
		}
		return turnBlock{Sym: "▶", Style: "user", Label: "USER", Body: lines, Open: true}
	case domain.EventQueued:
		return turnBlock{Sym: "▶", Style: "user", Label: "USER (queued)", Body: lines, Open: true}
	case domain.EventTask:
		body := lines
		if e.ToolDetail != "" {
			body = strings.Split(e.ToolDetail, "\n")
		}
		return turnBlock{Sym: "⤤", Style: "task", Label: strings.TrimSpace("TASK " + e.ToolArg), Body: body}
	case domain.EventAssistant:
		return turnBlock{Sym: "●", Style: "assistant", Label: "ASSISTANT", Body: lines, Open: true}
	case domain.EventReasoning:
		// Reasoning the agent recorded without plaintext (e.g. encrypted thinking)
		// carries no Text; the plugin describes it in ToolArg/ToolDetail instead.
		if strings.TrimSpace(text) == "" && e.ToolDetail != "" {
			return turnBlock{Sym: "◇", Style: "meta", Label: strings.TrimSpace("thinking " + e.ToolArg), Body: strings.Split(e.ToolDetail, "\n")}
		}
		return turnBlock{Sym: "◇", Style: "meta", Label: fmt.Sprintf("thinking (%d lines)", len(lines)), Body: lines}
	case domain.EventToolCall:
		return turnBlock{Sym: "◆", Style: "tool", Label: toolCallLabel(e), Body: toolBody(e, lines)}
	case domain.EventToolResult:
		lines = toolBody(e, lines)
		return turnBlock{Sym: "└", Style: "tool", Label: fmt.Sprintf("result (%d lines)", len(lines)), Body: lines}
	case domain.EventSystem:
		return turnBlock{Sym: "#", Style: "meta", Label: "system: " + one, Body: lines}
	case domain.EventAttachment:
		return turnBlock{Sym: "@", Style: "meta", Label: strings.TrimSpace("attachment " + fit(e.ToolArg, 70)), Body: lines}
	default:
		return turnBlock{Sym: "#", Style: "meta", Label: string(e.Kind) + ": " + one, Body: lines}
	}
}

// toolCallLabel renders a tool call's one-line label from the plugin-normalized
// ToolArg; nothing here inspects the agent-specific payload in Text.
func toolCallLabel(e domain.Event) string {
	name := e.ToolName
	if name == "" {
		name = "tool"
	}
	arg := fit(strings.Join(strings.Fields(e.ToolArg), " "), 70)
	return strings.TrimSpace(name + " " + arg)
}

// toolBody returns the expanded body: the plugin-normalized ToolDetail when
// present, otherwise the raw text lines.
func toolBody(e domain.Event, lines []string) []string {
	if e.ToolDetail != "" {
		return strings.Split(e.ToolDetail, "\n")
	}
	return lines
}

type turnLine struct {
	style  string
	text   string
	block  int
	header bool
	spans  []labelSpan // per-segment colors for the header line; text is their concatenation
}

func (m Model) turnFullLines() []turnLine {
	var out []turnLine
	// Wrap rather than truncate at the screen edge. Split at the same available width as
	// turnFullView's clip, expanding one segment into one display line. Continuation lines
	// get header=false so the block-start detection (turnBlockHeaderLine) is not broken.
	wrap := max(20, m.width-1)
	add := func(tl turnLine) {
		// Hanging indent: repeat the line's leading spaces on continuation segments so
		// wrapped text stays under the body column instead of running into the
		// timestamp gutter. Cap the indent so narrow screens keep room for content.
		lead := tl.text[:len(tl.text)-len(strings.TrimLeft(tl.text, " "))]
		if len(lead) > wrap/2 {
			lead = lead[:wrap/2]
		}
		for k, seg := range wrapWidth(strings.TrimPrefix(tl.text, lead), wrap-len(lead)) {
			t := tl
			t.text = lead + seg
			if k > 0 {
				t.header = false
			}
			out = append(out, t)
		}
	}
	for i, b := range m.turnBlocks {
		expanded := m.turnExpanded[i]
		marker := " "
		if len(b.Body) > 0 {
			if expanded {
				marker = "▾"
			} else {
				marker = "▸"
			}
		}
		// Timestamp gutter: HH:MM:SS for blocks that carry an event time, blank
		// (same width) for time-less blocks so the fold markers stay aligned.
		// NoGutter blocks (the edited-files section) render flush left instead.
		gutter := ""
		if !b.NoGutter {
			gutter = strings.Repeat(" ", 8) + " "
			if !b.Time.IsZero() {
				gutter = b.Time.Local().Format("15:04:05") + " "
			}
		}
		head := fmt.Sprintf("%s%s %s %s", gutter, marker, b.Sym, b.Label)
		var spans []labelSpan
		if len(b.LabelSpans) > 0 {
			spans = append(spans, labelSpan{fmt.Sprintf("%s%s %s %s", gutter, marker, b.Sym, b.LabelSpans[0].text), b.LabelSpans[0].style})
			spans = append(spans, b.LabelSpans[1:]...)
		}
		if !expanded && len(b.Body) > 0 && b.Sym != "◆" {
			if prev := oneLine(strings.Join(b.Body, "\n")); prev != "" {
				head += "  — " + prev
				if spans != nil {
					spans = append(spans, labelSpan{"  — " + prev, "plain"})
				}
			}
		}
		// The header is a one-line summary (with a preview when collapsed), so do not wrap it; truncate at the edge.
		out = append(out, turnLine{style: b.Style, text: head, block: i, header: true, spans: spans})
		if expanded {
			for _, ln := range b.Body {
				st := b.Style
				if strings.HasPrefix(b.Style, "diff") {
					st = diffLineStyle(ln)
				} else if strings.HasPrefix(ln, "+ ") {
					st = "add"
				} else if strings.HasPrefix(ln, "- ") {
					st = "del"
				} else if st != "user" && st != "assistant" {
					st = "plain"
				}
				// Indent past the gutter (if any) so the body stays under the label.
				add(turnLine{style: st, text: strings.Repeat(" ", len(gutter)+4) + ln, block: i})
			}
		}
	}
	return out
}

// turnFullView renders the turn named by row, which the caller must have taken
// from the cursor and checked to be a turn row. It takes the row rather than
// looking it up so it cannot fall back to detailView, which would call straight
// back here whenever turnOpen was set — an unbounded recursion, not a fallback.
func (m Model) turnFullView(row detailRow) string {
	lines := m.turnFullLines()
	bodyRows := m.turnBodyRows()
	offset := max(0, min(m.turnOffset, max(0, len(lines)-bodyRows)))
	end := min(len(lines), offset+bodyRows)
	var b strings.Builder
	s := m.detailSession
	turnNo := row.TurnIndex + 1
	events := m.turnEvents(row.Turn)
	head := " "
	if s != nil {
		if mk := strings.TrimSpace(statusMark(*s)); mk != "" {
			head += statusStyled(mk+" ", *s, false, true)
		}
		head += styled(s.AgentType+"  ", m.pluginColor(*s), false, true) + styled(shortID(s.SessionID)+"   ", lipgloss.Color("3"), false, false)
	}
	// Keep the element order consistent with the turn-list header (detailLead):
	// agent, id, time span, count, then model. detailLead shows the session span
	// and turn count; here it is this turn's span and its position.
	if len(events) > 0 {
		head += turnSpan(events) + "   "
	}
	head += fmt.Sprintf("turn #%d/%d", turnNo, len(m.detailTurns))
	// The model that produced this turn, matching the per-turn model column in
	// the turn list.
	if s != nil {
		if mdl := turnModel(events, s); mdl != "" {
			head += "   " + styled(mdl, lipgloss.Color("3"), false, false)
		}
	}
	b.WriteString(clip(head, max(20, m.width-1)) + "\n")
	for i := offset; i < end; i++ {
		line := lines[i]
		fg, bold := turnStyle(line.style)
		text := clip(line.text, max(20, m.width-1))
		switch {
		case i == m.turnCursor && len(line.spans) > 0:
			text = renderSpans(line.spans, max(20, m.width-1), true)
		case i == m.turnCursor:
			text = styled(padCol(text, max(1, m.width-1)), fg, true, bold)
		case m.turnFullQuery != "" && strings.Contains(strings.ToLower(line.text), strings.ToLower(m.turnFullQuery)):
			text = styled(text, roleColor("meta"), true, bold) // search hit (distinguished from the cursor by color)
		case len(line.spans) > 0:
			text = renderSpans(line.spans, max(20, m.width-1), false)
		default:
			text = styled(text, fg, false, bold)
		}
		b.WriteString(text + "\n")
	}
	for i := end - offset; i < bodyRows; i++ {
		b.WriteString("\n")
	}
	// A flash takes priority over the normal/search footer, as it does on the list
	// and detail screens; it reports why an editor could not be opened.
	if m.flash != "" {
		b.WriteString(flashBar(padCol(clip(m.flash, max(1, m.width-1)), max(1, m.width-1))))
		return b.String()
	}
	var foot string
	if m.turnFullSearching || m.turnFullQuery != "" {
		hits := m.turnFullHits(lines)
		pos := -1
		for i, h := range hits {
			if h == m.turnCursor {
				pos = i
				break
			}
		}
		foot = m.searchFooter(" Search > "+m.turnFullQuery, len(hits), pos)
	} else {
		foot = " ↑↓/jk move  Tab/[ ] block  →/← unfold/fold  z all  y copy  e open file  / search  q/← back "
	}
	b.WriteString(footer(padCol(clip(foot, max(1, m.width-1)), max(1, m.width-1))))
	return b.String()
}

// renderSpans renders a header line whose segments carry their own styles,
// clipping the joined text at width w (clip is not ANSI-aware, so each
// segment is clipped before styling). When selected, every segment keeps its
// foreground color and gets the cursor background, padded to the full width.
func renderSpans(spans []labelSpan, w int, selected bool) string {
	var b strings.Builder
	rem := w
	for _, sp := range spans {
		t := clip(sp.text, rem)
		if t == "" {
			break
		}
		fg, bold := turnStyle(sp.style)
		b.WriteString(styled(t, fg, selected, bold))
		rem -= runewidth.StringWidth(t)
		if rem <= 0 {
			break
		}
	}
	if selected && rem > 0 {
		b.WriteString(styled(strings.Repeat(" ", rem), "", true, false))
	}
	return b.String()
}

func turnStyle(style string) (lipgloss.Color, bool) {
	switch style {
	case "user":
		return roleColor("user"), true
	case "assistant":
		return roleColor("assistant"), true
	case "tool":
		return roleColor("tool"), false
	case "meta":
		return roleColor("meta"), false
	case "add", "diff-add":
		return roleColor("add"), false
	case "del", "diff-del":
		return roleColor("del"), false
	case "diff-mod":
		return roleColor("meta"), false
	}
	return lipgloss.Color(""), false
}
func (m *Model) ensureTurnOffset() {
	rows := m.turnBodyRows()
	if m.turnCursor < m.turnOffset {
		m.turnOffset = m.turnCursor
	}
	if m.turnCursor >= m.turnOffset+rows {
		m.turnOffset = m.turnCursor - rows + 1
	}
	m.turnOffset = max(0, min(m.turnOffset, max(0, len(m.turnFullLines())-rows)))
}
func (m Model) turnBodyRows() int {
	if m.height <= 0 {
		return max(1, len(m.turnFullLines()))
	}
	return max(1, m.height-2)
}
func (m Model) blockAtCursor() int {
	lines := m.turnFullLines()
	if m.turnCursor < 0 || m.turnCursor >= len(lines) {
		return -1
	}
	return lines[m.turnCursor].block
}
func (m *Model) toggleTurnBlock(i int) {
	if i < 0 || i >= len(m.turnBlocks) || len(m.turnBlocks[i].Body) == 0 {
		return
	}
	if m.turnExpanded[i] {
		delete(m.turnExpanded, i)
	} else {
		if m.turnExpanded == nil {
			m.turnExpanded = map[int]bool{}
		}
		m.turnExpanded[i] = true
	}
	if m.turnCursor >= len(m.turnFullLines()) {
		m.turnCursor = max(0, len(m.turnFullLines())-1)
	}
	m.ensureTurnOffset()
}
func (m *Model) toggleAllTurnBlocks() {
	if len(m.turnExpanded) > 0 {
		m.turnExpanded = map[int]bool{}
	} else {
		m.turnExpanded = map[int]bool{}
		for i, b := range m.turnBlocks {
			if len(b.Body) > 0 {
				m.turnExpanded[i] = true
			}
		}
	}
	m.ensureTurnOffset()
}
func (m *Model) ensureListOffset() {
	rows := m.listBodyRows()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+rows {
		m.offset = m.cursor - rows + 1
	}
	m.offset = max(0, min(m.offset, max(0, len(m.rows)-rows)))
}
func (m Model) listBodyRows() int {
	if m.height <= 0 {
		return max(1, len(m.rows))
	}
	// Row 0 = header, row 1 = search/status line (always reserved), last row = footer.
	// Changing the body row count based on whether search is active would shift the list
	// by one row as search is toggled. Row 1 is always reserved to keep the available
	// height constant, so this count must not depend on the search state either.
	return max(1, m.height-3)
}
func (m Model) detailBodyRows() int {
	if m.height <= 0 {
		return max(1, len(m.detailRows)*8)
	}
	// Row 0 = lead, row 1 = cwd, last row = footer. The head lines (the title or
	// the summary) are rows of the body, not of the header.
	return max(1, m.height-3)
}
func (m Model) turnEvents(ids []string) []domain.Event {
	return transcript.EventsOf(*m.detail, ids)
}

// turnRunningNow returns the current time for an in-progress turn (StatusRunning and the
// newest one), which turnMarkParts uses to extend the elapsed time live up to now. For
// any other turn it returns the zero value (so the elapsed time stops at its last event).
func (m Model) turnRunningNow(s *domain.Session, chronIndex int) time.Time {
	if s != nil && s.Status == domain.StatusRunning && chronIndex == m.detailNewestChron {
		return time.Now()
	}
	return time.Time{}
}

// turnMarkParts reads the displayed-path set from the Model rather than
// rebuilding it on every row of every frame.
func (m Model) turnMarkParts(ids []string, now time.Time) [9]string {
	events := m.turnEvents(ids)
	tools, replies, queued := 0, 0, 0
	tasks := 0
	for _, e := range events {
		switch e.Kind {
		case domain.EventToolCall:
			tools++
		case domain.EventAssistant:
			replies++
		case domain.EventQueued:
			queued++
		case domain.EventTask:
			tasks++
		}
	}
	trivial := 0
	if m.detail != nil {
		trivial, _ = convlogic.TurnBranches(*m.detail, ids, m.detailActive)
	}
	files, added, removed := editStats(events)
	var out [9]string
	out[0] = formatDuration(turnElapsed(events, now))
	if queued > 0 {
		out[1] = fmt.Sprintf("▶%d", queued)
	}
	if replies > 0 {
		out[2] = fmt.Sprintf("↩%d", replies)
	}
	if tools > 0 {
		out[3] = fmt.Sprintf("⚙%d", tools)
	}
	if tasks > 0 {
		out[4] = fmt.Sprintf("⤷%d", tasks)
	}
	if trivial > 0 {
		out[5] = fmt.Sprintf("↺%d", trivial)
	}
	if files > 0 {
		out[6] = fmt.Sprintf("*%d", files)
	}
	if added > 0 {
		out[7] = fmt.Sprintf("+%d", added)
	}
	if removed > 0 {
		out[8] = fmt.Sprintf("-%d", removed)
	}
	return out
}
func alignMark(s string, width int) string {
	if width <= 0 {
		return ""
	}
	s = clip(s, width)
	i := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			break
		}
		i += len(string(r))
	}
	icon, num := s[:i], s[i:]
	pad := width - runewidth.StringWidth(icon) - runewidth.StringWidth(num)
	return icon + strings.Repeat(" ", max(0, pad)) + num
}

// turnEndTime returns the largest (latest) timestamp among a turn's events (the max of the times).
func turnEndTime(events []domain.Event) time.Time {
	var t time.Time
	for _, e := range events {
		if e.Timestamp.IsZero() {
			continue
		}
		if t.IsZero() || e.Timestamp.After(t) {
			t = e.Timestamp
		}
	}
	return t
}
func turnDuration(events []domain.Event) time.Duration {
	return turnElapsed(events, time.Time{})
}

// turnElapsed returns a turn's elapsed time. When now is non-zero (an in-progress turn),
// the end time is max(latest event time, now), extending it live up to the present. When
// now is zero, it stops at the latest event time (a completed turn).
func turnElapsed(events []domain.Event, now time.Time) time.Duration {
	a, b := transcript.TurnTime(events), turnEndTime(events)
	if !now.IsZero() && now.After(b) {
		b = now
	}
	if a.IsZero() || b.IsZero() || !b.After(a) {
		return 0
	}
	return b.Sub(a)
}
func formatDuration(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		secs := int(d.Seconds())
		return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
	}
	if d < 24*time.Hour {
		mins := int(d.Minutes())
		return fmt.Sprintf("%dh%02dm", mins/60, mins%60)
	}
	hours := int(d.Hours())
	return fmt.Sprintf("%dd%02dh", hours/24, hours%24)
}
func turnSpan(events []domain.Event) string {
	a, b := transcript.TurnTime(events), turnEndTime(events)
	if a.IsZero() {
		return ""
	}
	out := a.Local().Format("2006-01-02 15:04")
	if !b.IsZero() && !b.Equal(a) {
		// Keep the date on the end too, matching the turn-list header
		// (detailLead) so the date does not vanish from the end of the span.
		out += " → " + b.Local().Format("2006-01-02 15:04")
	}
	return out
}

func editStats(events []domain.Event) (int, int, int) {
	files := map[string]bool{}
	added, removed := 0, 0
	for _, fc := range transcript.Changes(events) {
		files[fc.Path] = true
		added += fc.Added
		removed += fc.Removed
	}
	return len(files), added, removed
}

// diffLineStyle maps an apply_patch body line to a turnLine style name.
func diffLineStyle(ln string) string {
	switch {
	case strings.HasPrefix(ln, "+++") || strings.HasPrefix(ln, "---"):
		return "meta"
	case strings.HasPrefix(ln, "*** ") || strings.HasPrefix(ln, "@@"):
		return "meta"
	case strings.HasPrefix(ln, "+"):
		return "add"
	case strings.HasPrefix(ln, "-"):
		return "del"
	default:
		return "plain"
	}
}

func oneLine(text string) string {
	for _, ln := range strings.Split(text, "\n") {
		if s := strings.Join(strings.Fields(ln), " "); s != "" {
			return s
		}
	}
	return ""
}

// Run starts the TUI and returns the launch command to hand off after it exits.
// A command chosen via resume / fork is not started inside the TUI; instead bubbletea
// fully restores the terminal here and the caller execs it afterwards, avoiding a broken
// terminal handoff.
func Run(a *app.App, cached []domain.Session, db *cache.DB, afterScan func([]domain.Session)) (*domain.Command, error) {
	if term := os.Getenv("TERM"); term != "" && term != "dumb" {
		if strings.Contains(term, "256color") {
			lipgloss.SetColorProfile(termenv.ANSI256)
		} else {
			lipgloss.SetColorProfile(termenv.ANSI)
		}
	}
	m := New(a, cached, db)
	m.afterScan = afterScan
	final, e := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if e != nil {
		return nil, e
	}
	if m, ok := final.(Model); ok {
		return m.launch, nil
	}
	return nil, nil
}

// scanSucceeded says, per plugin, whether its scan came back without an error.
// A plugin that failed finds nothing, which must not be read as "every session
// of that agent was deleted".
func scanSucceeded(a *app.App, snap domain.Snapshot) map[string]bool {
	failed := map[string]bool{}
	for _, e := range snap.Errors {
		failed[e.PluginID] = true
	}
	ok := map[string]bool{}
	for _, p := range a.Catalog.Plugins {
		ok[p.ID] = !failed[p.ID]
	}
	return ok
}
