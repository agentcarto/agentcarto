package app

import (
	"context"
	"fmt"
	convlogic "github.com/agentcarto/core/conversation"
	"github.com/agentcarto/core/domain"
	"sort"
)

// NodeOrigin identifies, for a node of the synthesized (grafted) tree, the
// session that owns the node and the node's real ID within that session's own
// conversation. Grafting renames fork-child nodes to synthetic IDs (k0_…), so
// any operation that reaches back into a session's file (fork) must translate
// through this mapping first.
type NodeOrigin struct {
	Session domain.Session
	NodeID  string
}

func (a *App) ConversationWithForks(ctx context.Context, s domain.Session, sessions []domain.Session) (*domain.Conversation, error) {
	forks := buildForkMap(sessions)
	seen := map[domain.SessionKey]bool{}
	conv, _, _, err := a.conversationWithForks(ctx, s, forks, seen, domain.SessionKey{})
	return conv, err
}

// ConversationFromFocus resolves the root ancestor of focus within sessions and,
// starting from that root, returns the full tree with forks grafted in, together
// with the synthetic in-tree node ID (focusLeaf) corresponding to focus's active
// leaf and the node-origin map for the whole tree. If focus is itself the root
// (has no ancestor), focusLeaf is the root's active leaf. This is the entry
// point that canonicalizes the conversation view so that, however a session is
// opened, it shows the same "parent → … → current" tree; the TUI derives the
// focused branch from focusLeaf.
func (a *App) ConversationFromFocus(ctx context.Context, focus domain.Session, sessions []domain.Session) (*domain.Conversation, string, map[string]NodeOrigin, error) {
	root := rootAncestor(focus, sessions)
	forks := buildForkMap(sessions)
	seen := map[domain.SessionKey]bool{}
	return a.conversationWithForks(ctx, root, forks, seen, focus.Key())
}

// ForkAt plans a fork at a node identified by its origin (owning session +
// real node ID). KeepTurns is recomputed on the owner's own conversation with
// the same turn rules the display uses (TurnsOfPath), because turn numbers on
// the synthesized tree count the ancestors' turns too and do not match the
// owner's file.
func (a *App) ForkAt(ctx context.Context, origin NodeOrigin) (domain.MutationPlan, domain.Command, error) {
	conv, err := a.Conversation(ctx, origin.Session)
	if err != nil {
		return domain.MutationPlan{}, domain.Command{}, err
	}
	if conv == nil {
		return domain.MutationPlan{}, domain.Command{}, fmt.Errorf("conversation unavailable for %s", origin.Session.SessionID)
	}
	path := pathToNode(*conv, origin.NodeID)
	if len(path) == 0 {
		return domain.MutationPlan{}, domain.Command{}, fmt.Errorf("fork point %q not found in session %s", origin.NodeID, origin.Session.SessionID)
	}
	keep := len(convlogic.TurnsOfPath(*conv, path))
	return a.Fork(ctx, origin.Session, domain.ForkTarget{NodeID: origin.NodeID, KeepTurns: keep})
}

// pathToNode returns the root→id path within c, or nil when id is absent.
// A dangling parent link mid-chain ends the walk there (treated as the root).
func pathToNode(c domain.Conversation, id string) []string {
	if _, ok := c.Nodes[id]; !ok {
		return nil
	}
	var rev []string
	seen := map[string]bool{}
	for cur := id; cur != "" && !seen[cur]; {
		n, ok := c.Nodes[cur]
		if !ok {
			break
		}
		seen[cur] = true
		rev = append(rev, cur)
		cur = n.Parent
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// rootAncestor walks s's ParentSessionID chain as far back as it can be followed
// within sessions and returns the topmost ancestor. It stops where a parent is
// absent from sessions (filtered out or deleted) and treats that point as the root.
func rootAncestor(s domain.Session, sessions []domain.Session) domain.Session {
	byKey := make(map[string]domain.Session, len(sessions))
	for _, x := range sessions {
		byKey[x.PluginID+"|"+x.SessionID] = x
	}
	cur := s
	seen := map[string]bool{cur.PluginID + "|" + cur.SessionID: true}
	for cur.ParentSessionID != "" {
		key := cur.PluginID + "|" + cur.ParentSessionID
		p, ok := byKey[key]
		if !ok || seen[key] {
			break
		}
		seen[key] = true
		cur = p
	}
	return cur
}

func (a *App) conversationWithForks(ctx context.Context, s domain.Session, forks map[string][]domain.Session, seen map[domain.SessionKey]bool, focusKey domain.SessionKey) (*domain.Conversation, string, map[string]NodeOrigin, error) {
	k := s.Key()
	if seen[k] {
		conv, err := a.Conversation(ctx, s)
		return conv, "", identityOrigins(conv, s), err
	}
	seen[k] = true
	conv, err := a.Conversation(ctx, s)
	if err != nil {
		return conv, "", nil, err
	}
	if conv == nil {
		return conv, "", nil, nil
	}
	origins := identityOrigins(conv, s)
	focusLeaf := ""
	if k == focusKey {
		focusLeaf = conv.ActiveLeaf
	}
	children := forks[s.SessionID]
	for i, child := range children {
		childConv, childFocus, childOrigins, err := a.conversationWithForks(ctx, child, forks, seen, focusKey)
		if err != nil || childConv == nil {
			continue
		}
		want := childFocus
		if want == "" && child.Key() == focusKey {
			want = childConv.ActiveLeaf
		}
		grafted := map[string]string{} // grafted (renamed) ID -> ID in the child tree
		var mapped string
		if child.ForkAt != "" {
			mapped = graftClaudeFork(conv, *childConv, child.ForkAt, i, want, grafted)
		} else {
			mapped = graftFork(conv, *childConv, i, want, grafted)
		}
		// The child tree may itself contain grafts, so compose through its map.
		for nid, cid := range grafted {
			if o, ok := childOrigins[cid]; ok {
				origins[nid] = o
			}
		}
		if mapped != "" {
			focusLeaf = mapped
		}
	}
	sortConversationChildren(conv)
	return conv, focusLeaf, origins, nil
}

// identityOrigins maps every node of a session's own conversation to itself.
func identityOrigins(c *domain.Conversation, s domain.Session) map[string]NodeOrigin {
	if c == nil {
		return nil
	}
	out := make(map[string]NodeOrigin, len(c.Nodes))
	for id := range c.Nodes {
		out[id] = NodeOrigin{Session: s, NodeID: id}
	}
	return out
}

// MarkEmptyForks handles forks from plugins that do not record a fork point
// (codex/grok etc., where ForkAt is empty). It compares conversations to decide
// whether the child's active path is a strict prefix of the parent's (truncated and
// never continued, with zero unique content) and sets EmptyFork. claude forks carry
// ForkAt (the parent's last uuid) and are decided cheaply during Scan, so they are
// excluded here, which avoids re-reading large parent conversations. Forks marked
// EmptyFork are excluded from the listing.
func (a *App) MarkEmptyForks(ctx context.Context, sessions []domain.Session) []domain.Session {
	byKey := make(map[string]domain.Session, len(sessions))
	for _, s := range sessions {
		byKey[s.PluginID+"|"+s.SessionID] = s
	}
	for i := range sessions {
		s := &sessions[i]
		if s.ParentSessionID == "" || s.ForkAt != "" || s.EmptyFork {
			continue // non-fork / fork point already known (claude decided at Scan) / already marked empty
		}
		parent, ok := byKey[s.PluginID+"|"+s.ParentSessionID]
		if !ok {
			continue // parent not found: leave it undecided (keep it visible)
		}
		childConv, err := a.Conversation(ctx, *s)
		if err != nil || childConv == nil {
			continue
		}
		ca := childConv.ActivePath()
		if len(ca) == 0 {
			continue
		}
		parentConv, err := a.Conversation(ctx, parent)
		if err != nil || parentConv == nil {
			continue
		}
		// The child's entire active path matches a prefix of the parent's = no unique
		// continuation after the fork = empty.
		if forkSplit(*parentConv, *childConv) >= len(ca) {
			s.EmptyFork = true
		}
	}
	return sessions
}

func buildForkMap(sessions []domain.Session) map[string][]domain.Session {
	out := map[string][]domain.Session{}
	for _, s := range sessions {
		if s.ParentSessionID != "" {
			out[s.ParentSessionID] = append(out[s.ParentSessionID], s)
		}
	}
	for k := range out {
		sort.SliceStable(out[k], func(i, j int) bool {
			return out[k][i].StartedAt.Before(out[k][j].StartedAt)
		})
	}
	return out
}

func eventSig(c domain.Conversation, id string) [][2]string {
	n := c.Nodes[id]
	out := make([][2]string, 0, len(n.Events))
	for _, e := range n.Events {
		out = append(out, [2]string{string(e.Kind), e.Text})
	}
	return out
}

func sameEvents(a, b [][2]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func forkSplit(parent, child domain.Conversation) int {
	pa, ca := parent.ActivePath(), child.ActivePath()
	n := 0
	for i := 0; i < len(pa) && i < len(ca); i++ {
		if !sameEvents(eventSig(parent, pa[i]), eventSig(child, ca[i])) {
			break
		}
		n++
	}
	return n
}

// graftFork grafts child (a fork matched by shared prefix) onto parent. If want is a
// node ID within child, it returns the corresponding ID within parent after the graft
// (used to locate the focused branch; returns "" if want is outside the grafted
// subtree or empty). Every renamed node is recorded in grafted (new ID -> child ID).
func graftFork(parent *domain.Conversation, child domain.Conversation, idx int, want string, grafted map[string]string) string {
	pa, ca := parent.ActivePath(), child.ActivePath()
	split := forkSplit(*parent, child)
	if split == 0 || split >= len(ca) || split > len(pa) {
		return ""
	}
	attach := pa[split-1]
	graftRoot := ca[split]
	sub := subtreeIDs(child, graftRoot)
	prefix := fmt.Sprintf("k%d_", idx)
	for _, id := range sub {
		n := child.Nodes[id]
		nid := prefix + id
		par := attach
		if containsID(sub, n.Parent) {
			par = prefix + n.Parent
		}
		parent.Nodes[nid] = domain.ConvNode{ID: nid, Parent: par, Timestamp: n.Timestamp, Events: append([]domain.Event(nil), n.Events...)}
		parent.Children[par] = append(parent.Children[par], nid)
		grafted[nid] = id
	}
	parent.ForkRoots = append(parent.ForkRoots, prefix+graftRoot)
	if want != "" && containsID(sub, want) {
		return prefix + want
	}
	return ""
}

// graftClaudeFork grafts child (a claude fork that records its fork point, attach)
// onto parent. Nodes already present in parent are skipped, so the shared base is not
// duplicated. If want is a node ID within child, it returns the corresponding ID
// within parent after the graft (used to locate the focused branch; returns "" if
// want is empty). Every renamed node is recorded in grafted (new ID -> child ID);
// nodes inherited from parent keep their IDs and their parent origin.
func graftClaudeFork(parent *domain.Conversation, child domain.Conversation, attach string, idx int, want string, grafted map[string]string) string {
	if _, ok := parent.Nodes[attach]; !ok || len(child.Nodes) == 0 {
		return ""
	}
	prefix := fmt.Sprintf("k%d_", idx)
	ids := make([]string, 0, len(child.Nodes))
	for id := range child.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, exists := parent.Nodes[id]; exists {
			continue
		}
		n := child.Nodes[id]
		nid := prefix + id
		par := attach
		if _, ok := child.Nodes[n.Parent]; ok {
			if _, exists := parent.Nodes[n.Parent]; exists {
				par = n.Parent
			} else {
				par = prefix + n.Parent
			}
		}
		parent.Nodes[nid] = domain.ConvNode{ID: nid, Parent: par, Timestamp: n.Timestamp, Events: append([]domain.Event(nil), n.Events...)}
		parent.Children[par] = append(parent.Children[par], nid)
		grafted[nid] = id
		if par == attach || par == n.Parent {
			parent.ForkRoots = append(parent.ForkRoots, nid)
		}
	}
	if want == "" {
		return ""
	}
	// Nodes inherited from parent keep their original IDs (no prefix); nodes unique to
	// child were taken in with the prefix.
	if _, exists := parent.Nodes[want]; exists {
		return want // inherited from parent (the base up to attach)
	}
	return prefix + want
}

func subtreeIDs(c domain.Conversation, root string) []string {
	var out []string
	stack := []string{root}
	seen := map[string]bool{}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		stack = append(stack, c.Children[id]...)
	}
	sort.Strings(out)
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func sortConversationChildren(c *domain.Conversation) {
	for parent := range c.Children {
		sort.SliceStable(c.Children[parent], func(i, j int) bool {
			a, b := c.Nodes[c.Children[parent][i]], c.Nodes[c.Children[parent][j]]
			if !a.Timestamp.Equal(b.Timestamp) {
				if a.Timestamp.IsZero() {
					return false
				}
				if b.Timestamp.IsZero() {
					return true
				}
				return a.Timestamp.Before(b.Timestamp)
			}
			// ID as the tiebreak: the previous map-iteration order was random,
			// so equal-timestamp siblings shuffled on every reload.
			return a.ID < b.ID
		})
	}
}
