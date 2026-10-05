package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestInbox_CtrlDPagesDownLikeD pins that ctrl+d (vim half-page habit) pages the
// inbox list down exactly like plain d.
func TestInbox_CtrlDPagesDownLikeD(t *testing.T) {
	m := cmdModel(t)
	m.height = 5 // inboxPageStep → 10, clamps to last item
	m.inbox.Select(0)

	res, _ := m.updateInbox(tea.KeyMsg{Type: tea.KeyCtrlD})
	got := res.(Model).inbox.Index()

	m2 := cmdModel(t)
	m2.height = 5
	m2.inbox.Select(0)
	res2, _ := m2.updateInbox(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	want := res2.(Model).inbox.Index()

	if got != want || got == 0 {
		t.Fatalf("ctrl+d index=%d, d index=%d; want equal and > 0", got, want)
	}
}

// TestInbox_CtrlUPagesUpLikeU pins that ctrl+u (vim half-page habit) pages the
// inbox list up exactly like plain u — also while emails are marked (marks stay).
func TestInbox_CtrlUPagesUpLikeU(t *testing.T) {
	m := cmdModel(t)
	m.height = 5
	last := len(m.inbox.Items()) - 1
	m.inbox.Select(last)
	m.markedUIDs[m.emails[0].UID] = true

	res, _ := m.updateInbox(tea.KeyMsg{Type: tea.KeyCtrlU})
	got := res.(Model).inbox.Index()
	if len(res.(Model).markedUIDs) != 1 {
		t.Fatalf("ctrl+u must not touch marks, got %d marked", len(res.(Model).markedUIDs))
	}

	m2 := cmdModel(t)
	m2.height = 5
	m2.inbox.Select(last)
	res2, _ := m2.updateInbox(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("u")})
	want := res2.(Model).inbox.Index()

	if got != want || got == last {
		t.Fatalf("ctrl+u index=%d, u index=%d; want equal and < %d", got, want, last)
	}
}

// TestInbox_EscClearsMarks pins that esc took over "clear all marks" from ctrl+u:
// marks are cleared first (before any filter/search is dismissed) and the cursor
// does not move.
func TestInbox_EscClearsMarks(t *testing.T) {
	m := cmdModel(t)
	m.height = 5
	last := len(m.inbox.Items()) - 1
	m.inbox.Select(last)
	m.markedUIDs[m.emails[0].UID] = true

	res, _ := m.updateInbox(tea.KeyMsg{Type: tea.KeyEsc})
	got := res.(Model)

	if len(got.markedUIDs) != 0 {
		t.Fatalf("esc with marks: want marks cleared, got %d", len(got.markedUIDs))
	}
	if got.inbox.Index() != last {
		t.Fatalf("esc with marks: cursor moved to %d, want %d", got.inbox.Index(), last)
	}
}
