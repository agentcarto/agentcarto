package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/agentcarto/core/domain"
)

func detailModelWithFlash() Model {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Events: []domain.Event{{Kind: domain.EventUser, Text: "q"}}},
	})
	s := domain.Session{PluginID: "claude", SessionID: "s"}
	return Model{detail: &c, detailSession: &s, flash: "Jumped to parent: abc1234 (q to go back)"}
}

// After a parent jump, pressing q closes the detail view and returns to the list; the footer notice (flash) is not carried over to the list.
func TestQuitDetailToListClearsFlash(t *testing.T) {
	m := detailModelWithFlash()
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := u.(Model)
	if m2.detail != nil {
		t.Fatalf("detail should be closed (back to list)")
	}
	if m2.flash != "" {
		t.Fatalf("flash should be cleared on return to list, got %q", m2.flash)
	}
}

// After a parent jump, when q returns to the fork child, the stale "Jumped to parent" notice is cleared too.
func TestForkBackClearsFlash(t *testing.T) {
	m := detailModelWithFlash()
	m.forkBack = []domain.Session{{PluginID: "claude", SessionID: "child"}}
	u, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	m2 := u.(Model)
	if m2.detailSession == nil || m2.detailSession.SessionID != "child" {
		t.Fatalf("should return to fork child, got %+v", m2.detailSession)
	}
	if m2.flash != "" {
		t.Fatalf("flash should be cleared when returning to child, got %q", m2.flash)
	}
}

// When a flash is set, flashAt is recorded and an expiry timer (cmd) is scheduled.
func TestFlashSetSchedulesExpiry(t *testing.T) {
	before := time.Now()
	u, cmd := Model{}.Update(convMsg{e: errors.New("nope")})
	m := u.(Model)
	if m.flash != "nope" {
		t.Fatalf("flash not set: %q", m.flash)
	}
	if m.flashAt.Before(before) {
		t.Fatal("flashAt should be recorded when flash is set")
	}
	if cmd == nil {
		t.Fatal("expected an expiry tick to be scheduled")
	}
}

// After flashTTL has elapsed, an expiry message clears the flash.
func TestFlashExpiresAfterTTL(t *testing.T) {
	m := Model{flash: "boom", flashAt: time.Now().Add(-flashTTL - time.Second)}
	u, _ := m.Update(flashExpireMsg{})
	if got := u.(Model).flash; got != "" {
		t.Fatalf("expired flash should be cleared, got %q", got)
	}
}

// While a newer flash has re-armed the timer, a stale expiry timer must not clear it.
func TestFlashExpireKeepsRecentMessage(t *testing.T) {
	m := Model{flash: "newer", flashAt: time.Now()}
	u, _ := m.Update(flashExpireMsg{})
	if got := u.(Model).flash; got != "newer" {
		t.Fatalf("recent flash should survive a stale expire timer, got %q", got)
	}
}

// The flash is rendered in both the list and detail footers (the detail view previously did not show a flash).
func TestFlashRendersInBothFooters(t *testing.T) {
	c := domain.NewConversation([]domain.ConvNode{
		{ID: "u", Events: []domain.Event{{Kind: domain.EventUser, Text: "hi"}}},
	})
	s := domain.Session{PluginID: "claude", SessionID: "s"}

	dm := Model{width: 80, height: 20, detailSession: &s}
	u, _ := dm.Update(convMsg{c: &c, reset: true})
	dm = u.(Model)
	dm.flash = "detail boom"
	if !strings.Contains(dm.View(), "detail boom") {
		t.Fatal("detail footer should render flash (was previously invisible)")
	}

	lm := Model{width: 80, height: 20, flash: "list boom"}
	if !strings.Contains(lm.View(), "list boom") {
		t.Fatal("list footer should render flash")
	}
}
