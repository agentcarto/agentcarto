package app

import (
	"context"
	"github.com/agentcarto/agentcarto/internal/catalog"
	convlogic "github.com/agentcarto/core/conversation"
	"github.com/agentcarto/core/domain"
	"github.com/agentcarto/core/plugin"
	"testing"
	"time"
)

type convLoaderStub struct {
	convs map[string]domain.Conversation
}

func (s convLoaderStub) LoadConversation(_ context.Context, r domain.SessionRef) (*domain.Conversation, error) {
	c := s.convs[r.Source]
	return &c, nil
}

func testAppWithConvs(convs map[string]domain.Conversation) *App {
	return &App{Catalog: catalog.Catalog{Plugins: []plugin.Instance{{
		ID:         "p",
		Descriptor: plugin.Descriptor{Capabilities: domain.Capabilities{Conversation: true}},
		Impl:       convLoaderStub{convs: convs},
	}}}}
}

func TestConversationWithForksGraftsSharedPrefixFork(t *testing.T) {
	parent := domain.NewConversation([]domain.ConvNode{
		{ID: "p1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "test1"}}},
		{ID: "p2", Parent: "p1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a1"}}},
	})
	child := domain.NewConversation([]domain.ConvNode{
		{ID: "c1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "test1"}}},
		{ID: "c2", Parent: "c1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a1"}}},
		{ID: "c3", Parent: "c2", Timestamp: time.Unix(3, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "fork"}}},
		{ID: "c4", Parent: "c3", Timestamp: time.Unix(4, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "af"}}},
	})
	a := testAppWithConvs(map[string]domain.Conversation{"parent": parent, "child": child})
	conv, err := a.ConversationWithForks(context.Background(),
		domain.Session{PluginID: "p", SessionID: "P", SourceRef: domain.SessionRef{Source: "parent"}},
		[]domain.Session{
			{PluginID: "p", SessionID: "P", SourceRef: domain.SessionRef{Source: "parent"}},
			{PluginID: "p", SessionID: "C", ParentSessionID: "P", StartedAt: time.Unix(10, 0), SourceRef: domain.SessionRef{Source: "child"}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := convlogic.BranchLead(*conv, conv.ForkRoots[0]); got != "▶ fork" {
		t.Fatalf("fork lead=%q roots=%v nodes=%#v", got, conv.ForkRoots, conv.Nodes)
	}
	if got := convlogic.TurnHeadline(*conv, conv.ActivePath()); got != "test1" {
		t.Fatalf("parent active path should remain active, headline=%q", got)
	}
}

// For forks from codex/grok etc. that do not record a fork point (ForkAt empty):
// set EmptyFork when the child's active path is a strict prefix of the parent's
// (not continued), and do not set it when there is unique continuation.
func TestMarkEmptyForksDiffBased(t *testing.T) {
	parent := domain.NewConversation([]domain.ConvNode{
		{ID: "p1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "q1"}}},
		{ID: "p2", Parent: "p1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a1"}}},
		{ID: "p3", Parent: "p2", Timestamp: time.Unix(3, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "q2"}}},
	})
	// empty fork: exactly the parent's prefix (through q1,a1) with no unique continuation.
	empty := domain.NewConversation([]domain.ConvNode{
		{ID: "e1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "q1"}}},
		{ID: "e2", Parent: "e1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a1"}}},
	})
	// continued fork: a unique message q9 after the shared prefix.
	cont := domain.NewConversation([]domain.ConvNode{
		{ID: "c1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "q1"}}},
		{ID: "c2", Parent: "c1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a1"}}},
		{ID: "c3", Parent: "c2", Timestamp: time.Unix(3, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "q9"}}},
	})
	a := testAppWithConvs(map[string]domain.Conversation{"parent": parent, "empty": empty, "cont": cont})
	sessions := []domain.Session{
		{PluginID: "p", SessionID: "P", SourceRef: domain.SessionRef{Source: "parent"}},
		{PluginID: "p", SessionID: "E", ParentSessionID: "P", SourceRef: domain.SessionRef{Source: "empty"}},
		{PluginID: "p", SessionID: "C", ParentSessionID: "P", SourceRef: domain.SessionRef{Source: "cont"}},
	}
	got := a.MarkEmptyForks(context.Background(), sessions)
	byID := map[string]bool{}
	for _, s := range got {
		byID[s.SessionID] = s.EmptyFork
	}
	if !byID["E"] {
		t.Fatal("prefix-only fork should be EmptyFork")
	}
	if byID["C"] {
		t.Fatal("continued fork should not be EmptyFork")
	}
	if byID["P"] {
		t.Fatal("parent (non-fork) should not be EmptyFork")
	}
}

// Forks with ForkAt (claude decides them at Scan time) are excluded from MarkEmptyForks.
func TestMarkEmptyForksSkipsForkAtSessions(t *testing.T) {
	parent := domain.NewConversation([]domain.ConvNode{
		{ID: "p1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "q1"}}},
	})
	a := testAppWithConvs(map[string]domain.Conversation{"parent": parent, "child": parent})
	sessions := []domain.Session{
		{PluginID: "p", SessionID: "P", SourceRef: domain.SessionRef{Source: "parent"}},
		{PluginID: "p", SessionID: "C", ParentSessionID: "P", ForkAt: "p1", SourceRef: domain.SessionRef{Source: "child"}},
	}
	got := a.MarkEmptyForks(context.Background(), sessions)
	for _, s := range got {
		if s.SessionID == "C" && s.EmptyFork {
			t.Fatal("ForkAt session must be skipped by MarkEmptyForks (handled at Scan)")
		}
	}
}

func TestConversationWithForksGraftsClaudeForkAtUUID(t *testing.T) {
	parent := domain.NewConversation([]domain.ConvNode{
		{ID: "p1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "test1"}}},
		{ID: "p2", Parent: "p1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a1"}}},
	})
	child := domain.NewConversation([]domain.ConvNode{
		{ID: "f1", Timestamp: time.Unix(3, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "forkdir"}}},
		{ID: "f2", Parent: "f1", Timestamp: time.Unix(4, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "fa"}}},
	})
	a := testAppWithConvs(map[string]domain.Conversation{"parent": parent, "child": child})
	conv, err := a.ConversationWithForks(context.Background(),
		domain.Session{PluginID: "p", SessionID: "P", SourceRef: domain.SessionRef{Source: "parent"}},
		[]domain.Session{
			{PluginID: "p", SessionID: "P", SourceRef: domain.SessionRef{Source: "parent"}},
			{PluginID: "p", SessionID: "C", ParentSessionID: "P", ForkAt: "p2", StartedAt: time.Unix(10, 0), SourceRef: domain.SessionRef{Source: "child"}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.ForkRoots) != 1 {
		t.Fatalf("fork roots=%v", conv.ForkRoots)
	}
	if got := convlogic.BranchLead(*conv, conv.ForkRoots[0]); got != "▶ forkdir" {
		t.Fatalf("fork lead=%q roots=%v nodes=%#v", got, conv.ForkRoots, conv.Nodes)
	}
}

// Opening a fork session in focus builds the whole tree starting from the root
// ancestor (parent), with focusLeaf pointing at the fork branch's leaf. The parent
// mainline is active (primary) and the fork branch is classified "fork". This is the
// heart of conversation-view canonicalization.
func TestConversationFromFocusRootsAtAncestorAndFocusesFork(t *testing.T) {
	parent := domain.NewConversation([]domain.ConvNode{
		{ID: "p1", Timestamp: time.Unix(1, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "test1"}}},
		{ID: "p2", Parent: "p1", Timestamp: time.Unix(2, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a1"}}},
	})
	child := domain.NewConversation([]domain.ConvNode{
		{ID: "f1", Timestamp: time.Unix(3, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "forkdir"}}},
		{ID: "f2", Parent: "f1", Timestamp: time.Unix(4, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "fa"}}},
	})
	a := testAppWithConvs(map[string]domain.Conversation{"parent": parent, "child": child})
	sessions := []domain.Session{
		{PluginID: "p", SessionID: "P", SourceRef: domain.SessionRef{Source: "parent"}},
		{PluginID: "p", SessionID: "C", ParentSessionID: "P", ForkAt: "p2", StartedAt: time.Unix(10, 0), SourceRef: domain.SessionRef{Source: "child"}},
	}
	// even when opening the fork child (C), build starting from the root ancestor (P=parent).
	conv, focusLeaf, origins, err := a.ConversationFromFocus(context.Background(), sessions[1], sessions)
	if err != nil {
		t.Fatal(err)
	}
	// Grafted nodes resolve back to the child session with their real IDs; the
	// parent's own nodes resolve to the parent. Fork planning depends on this
	// mapping because the grafted IDs (k0_…) do not exist in any session file.
	if o := origins["k0_f2"]; o.Session.SessionID != "C" || o.NodeID != "f2" {
		t.Fatalf("origin of k0_f2 = %+v, want session C node f2", o)
	}
	if o := origins["p2"]; o.Session.SessionID != "P" || o.NodeID != "p2" {
		t.Fatalf("origin of p2 = %+v, want session P node p2", o)
	}
	// the parent mainline is the active path (primary).
	if got := convlogic.TurnHeadline(*conv, conv.ActivePath()); got != "test1" {
		t.Fatalf("parent should remain active, headline=%q", got)
	}
	// focusLeaf is the fork branch's leaf (the in-tree ID of child's active leaf f2).
	if focusLeaf != "k0_f2" {
		t.Fatalf("focusLeaf=%q want k0_f2", focusLeaf)
	}
	// the branch root of focusLeaf is classified "fork" in ForkRoots (not main).
	if len(conv.ForkRoots) != 1 || convlogic.BranchKind(*conv, conv.ForkRoots[0]) != "fork" {
		t.Fatalf("fork branch not classified: roots=%v", conv.ForkRoots)
	}
	if conv.Nodes[focusLeaf].Parent != conv.ForkRoots[0] {
		t.Fatalf("focusLeaf %q parent=%q want fork root %q", focusLeaf, conv.Nodes[focusLeaf].Parent, conv.ForkRoots[0])
	}
}

// rewinderStub records the ForkTarget it receives (and loads conversations).
type rewinderStub struct {
	convLoaderStub
	got *domain.ForkTarget
	sid *string
}

func (s rewinderStub) PlanFork(_ context.Context, sess domain.Session, t domain.ForkTarget) (domain.MutationPlan, domain.Command, error) {
	*s.got = t
	*s.sid = sess.SessionID
	return domain.MutationPlan{}, domain.Command{}, nil
}

// ForkAt recomputes KeepTurns within the owning session's own conversation:
// forking at the fork child's second turn must send the child session and a
// turn count in the child's numbering, not the synthesized tree's.
func TestForkAtUsesOwnerSessionAndOwnerTurnNumbering(t *testing.T) {
	child := domain.NewConversation([]domain.ConvNode{
		{ID: "f1", Timestamp: time.Unix(3, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "turn one"}}},
		{ID: "f2", Parent: "f1", Timestamp: time.Unix(4, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "a"}}},
		{ID: "f3", Parent: "f2", Timestamp: time.Unix(5, 0), Events: []domain.Event{{Kind: domain.EventUser, Text: "turn two"}}},
		{ID: "f4", Parent: "f3", Timestamp: time.Unix(6, 0), Events: []domain.Event{{Kind: domain.EventAssistant, Text: "b"}}},
	})
	var got domain.ForkTarget
	var sid string
	a := &App{Catalog: catalog.Catalog{Plugins: []plugin.Instance{{
		ID:         "p",
		Descriptor: plugin.Descriptor{Capabilities: domain.Capabilities{Conversation: true, Rewind: true}},
		Impl:       rewinderStub{convLoaderStub: convLoaderStub{convs: map[string]domain.Conversation{"child": child}}, got: &got, sid: &sid},
	}}}}
	owner := domain.Session{PluginID: "p", SessionID: "C", SourceRef: domain.SessionRef{Source: "child"}}
	if _, _, e := a.ForkAt(context.Background(), NodeOrigin{Session: owner, NodeID: "f4"}); e != nil {
		t.Fatal(e)
	}
	if sid != "C" {
		t.Fatalf("fork sent to session %q, want the owning session C", sid)
	}
	if got.NodeID != "f4" || got.KeepTurns != 2 {
		t.Fatalf("ForkTarget=%+v, want NodeID f4 KeepTurns 2", got)
	}
	// An unknown node is rejected instead of being passed through to the plugin.
	if _, _, e := a.ForkAt(context.Background(), NodeOrigin{Session: owner, NodeID: "k0_f4"}); e == nil {
		t.Fatal("want error for a node ID that does not exist in the owner's conversation")
	}
}
