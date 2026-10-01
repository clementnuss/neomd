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
