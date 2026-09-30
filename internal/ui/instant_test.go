package ui

// Workflow tests for the instant-IMAP layers: background connection,
// optimistic list updates and the per-folder cache. They drive the real
// bubbletea Update handlers with real messages; no network.

import (
	"testing"

	"github.com/sspaeti/neomd/internal/config"
	"github.com/sspaeti/neomd/internal/screener"
)

// instantModel builds a one-account inbox model with n INBOX emails loaded
// (UIDs 1..n, newest = highest UID) and the cursor on the first row.
func instantModel(t *testing.T, n int) Model {
	t.Helper()
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "P", From: "me@x"}}}
	cfg.Folders = config.FoldersConfig{Inbox: "INBOX", ToScreen: "ToScreen", Feed: "Feed", PaperTrail: "PaperTrail",
		Archive: "Archive", Waiting: "Waiting", Someday: "Someday", Scheduled: "Scheduled", Sent: "Sent", Trash: "Trash",
		ScreenedOut: "ScreenedOut", Drafts: "Drafts", Spam: "Spam"}
	dir := t.TempDir()
	sc, err := screener.New(screener.Config{
		ScreenedIn: dir + "/in.txt", ScreenedOut: dir + "/out.txt", Feed: dir + "/feed.txt",
		PaperTrail: dir + "/paper.txt", Spam: dir + "/spam.txt"})
	if err != nil {
		t.Fatal(err)
	}
	m := Model{cfg: cfg, accounts: cfg.ActiveAccounts(), folders: cfg.Folders.TabLabels(), screener: sc,
		inbox: newInboxList(120, 30, "Sent", "Drafts"), markedUIDs: map[uint32]bool{}, spyPixelKeys: map[string]bool{},
		sortField: "date", sortReverse: true, width: 120, height: 40}
	for i := n; i >= 1; i-- {
		e := mkEmail(uint32(i), "<m"+string(rune('0'+i))+"@x>", "s", "Sender <s@example.com>", n-i, true)
		e.Folder = "INBOX"
		m.emails = append(m.emails, e)
	}
	m.applyFilter()
	m.inbox.Select(0)
	return m
}

func uidsInList(m Model) []uint32 {
	var out []uint32
	for _, it := range m.inbox.Items() {
		out = append(out, it.(emailItem).email.UID)
	}
	return out
}

func TestInboxLoadSkipsAutoScreenWhileBgSyncRuns(t *testing.T) {
	m := instantModel(t, 2)
	// Sender is on the screened-out list, so a normal Inbox load would move it.
	if err := m.screener.Block("Sender <s@example.com>"); err != nil {
		t.Fatal(err)
	}
	m.bgSyncInProgress = true
	res, _ := m.Update(emailsLoadedMsg{emails: m.emails, folder: "INBOX"})
	mm := res.(Model)
	if mm.loading || mm.bulkProgress != nil {
		t.Errorf("auto-screen must be skipped while bg sync runs: loading=%v bulk=%v", mm.loading, mm.bulkProgress)
	}
}
