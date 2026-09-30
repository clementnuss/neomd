package ui

// Workflow tests for the instant-IMAP layers: background connection,
// optimistic list updates and the per-folder cache. They drive the real
// bubbletea Update handlers with real messages; no network.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sspaeti/neomd/internal/config"
	"github.com/sspaeti/neomd/internal/imap"
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

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestOptimistic_DeleteRemovesRowImmediately(t *testing.T) {
	m := instantModel(t, 3) // list: 3,2,1 ; cursor on uid 3
	res, cmd := m.updateInbox(key("x"))
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("x must still fire the batch move command")
	}
	if mm.loading {
		t.Error("list must stay visible (loading=false)")
	}
	if got := uidsInList(mm); len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Errorf("list after x = %v, want [2 1]", got)
	}
	if e := selectedEmail(mm.inbox); e == nil || e.UID != 2 {
		t.Errorf("cursor should land on the next row (uid 2), got %+v", e)
	}
	if len(mm.markedUIDs) != 0 {
		t.Error("marks must be cleared")
	}
}

func TestOptimistic_BatchDoneSuccessRefreshesInBackground(t *testing.T) {
	m := instantModel(t, 3)
	res, _ := m.updateInbox(key("x"))
	mm := res.(Model)
	res, cmd := mm.Update(batchDoneMsg{undo: []undoMove{{uid: 9, fromFolder: "INBOX", toFolder: "Trash"}}})
	mm = res.(Model)
	if mm.loading {
		t.Error("success must not hide the list")
	}
	if !mm.refreshing || cmd == nil {
		t.Error("success must start a background refresh (refreshing=true, cmd!=nil)")
	}
	if len(mm.undoStack) != 1 || mm.undoStack[0][0].uid != 9 {
		t.Errorf("undo not pushed: %+v", mm.undoStack)
	}
	if mm.status != "Moved." {
		t.Errorf("status = %q", mm.status)
	}
}

func TestOptimistic_BatchErrorReloadsAndKeepsPartialUndo(t *testing.T) {
	m := instantModel(t, 3)
	res, _ := m.updateInbox(key("x"))
	mm := res.(Model)
	res, cmd := mm.Update(batchDoneMsg{err: testErr("MOVE 3 → Trash: NO"), undo: []undoMove{{uid: 5, fromFolder: "INBOX", toFolder: "Trash"}}})
	mm = res.(Model)
	if !mm.loading || cmd == nil {
		t.Error("an error must trigger a full server reload (loading=true, cmd!=nil)")
	}
	if !mm.isError || mm.status == "" {
		t.Error("error must be visible in the status line")
	}
	if len(mm.undoStack) != 1 || mm.undoStack[0][0].uid != 5 {
		t.Errorf("partial undo lost: %+v", mm.undoStack)
	}
}

func TestOptimistic_ScreenInToScreenRemovesSameSender(t *testing.T) {
	m := instantModel(t, 4)
	m.activeFolderI = 1 // ToScreen tab
	for i := range m.emails {
		m.emails[i].Folder = "ToScreen"
	}
	m.emails[1].From = "Other <o@example.com>" // uid 3
	m.applyFilter()
	m.inbox.Select(0) // uid 4, Sender <s@example.com>
	res, cmd := m.updateInbox(key("I"))
	mm := res.(Model)
	if cmd == nil || mm.loading {
		t.Fatalf("I must fire the screener cmd and keep the list: cmd=%v loading=%v", cmd != nil, mm.loading)
	}
	if got := uidsInList(mm); len(got) != 1 || got[0] != 3 {
		t.Errorf("only the other sender should remain, got %v", got)
	}
}

func TestOptimistic_ScreenWithMarksRemovesOnlyTargets(t *testing.T) {
	m := instantModel(t, 3)
	m.markedUIDs[2] = true
	res, _ := m.updateInbox(key("O"))
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Errorf("list = %v, want [3 1]", got)
	}
}

func TestOptimistic_AutoScreenMovesHiddenBeforeFirstDraw(t *testing.T) {
	m := instantModel(t, 3)
	m.emails = nil
	m.applyFilter()
	if err := m.screener.Block("Sender <s@example.com>"); err != nil {
		t.Fatal(err)
	}
	// Screened-in, so it stays in Inbox (an unknown sender would go to ToScreen).
	if err := m.screener.Approve("Friend <f@example.com>"); err != nil {
		t.Fatal(err)
	}
	blocked := mkEmail(7, "<b@x>", "spam", "Sender <s@example.com>", 0, false)
	blocked.Folder = "INBOX"
	ok := mkEmail(8, "<o@x>", "fine", "Friend <f@example.com>", 0, false)
	ok.Folder = "INBOX"
	res, cmd := m.Update(emailsLoadedMsg{emails: []imap.Email{ok, blocked}, folder: "INBOX"})
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("moves must be fired")
	}
	if mm.loading {
		t.Error("list must not be hidden during auto-screen moves")
	}
	if got := uidsInList(mm); len(got) != 1 || got[0] != 8 {
		t.Errorf("blocked mail must be gone before first draw, list = %v", got)
	}
	res, _ = mm.Update(autoScreenDoneMsg{moved: 1, err: testErr("MOVE NO")})
	if !res.(Model).loading {
		t.Error("auto-screen error must reload from server")
	}
}

func TestOptimistic_BulkProgressShownInStatusNotSpinner(t *testing.T) {
	m := instantModel(t, 2)
	m.bulkProgress = &bulkOp{label: "Moving", total: 20}
	m.bulkProgress.moved.Store(3)
	out := m.View()
	if !strings.Contains(out, "Moving: 3/20") {
		t.Errorf("bulk progress missing from view:\n%s", out)
	}
	if strings.Contains(out, "Loading…") {
		t.Error("list must not be replaced by the loading spinner")
	}
}

// Handlers that still hide the list (U undo, X, toggle-seen, y-confirmed
// :screen) must keep the spinner reload on success: their result is not
// known locally, so a visible stale list would show wrong rows.
func TestOptimistic_SpinnerPathsKeepSpinnerReload(t *testing.T) {
	m := instantModel(t, 2)
	m.loading = true // e.g. U undo in flight
	res, cmd := m.Update(batchDoneMsg{})
	mm := res.(Model)
	if !mm.loading || mm.refreshing || cmd == nil || mm.status != "Done." {
		t.Errorf("batchDone after spinner op: loading=%v refreshing=%v status=%q", mm.loading, mm.refreshing, mm.status)
	}
	m.loading = true // y-confirmed :screen in flight
	res, cmd = m.Update(autoScreenDoneMsg{moved: 2})
	mm = res.(Model)
	if !mm.loading || mm.refreshing || cmd == nil {
		t.Errorf("autoScreenDone after spinner op: loading=%v refreshing=%v", mm.loading, mm.refreshing)
	}
}

func TestOptimistic_CursorStaysOnSameEmailWhenRowsAboveRemoved(t *testing.T) {
	m := instantModel(t, 6) // list: 6,5,4,3,2,1
	m.markedUIDs[6] = true
	m.markedUIDs[5] = true
	m.applyFilter()
	m.inbox.Select(3) // uid 3, unmarked
	res, _ := m.updateInbox(key("A"))
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 4 || got[0] != 4 {
		t.Fatalf("list = %v, want [4 3 2 1]", got)
	}
	if e := selectedEmail(mm.inbox); e == nil || e.UID != 3 {
		t.Errorf("cursor must stay on uid 3, got %+v", e)
	}
}

func TestOptimistic_CursorLandsOnNextRowAfterSenderExpansionAbove(t *testing.T) {
	m := instantModel(t, 6) // list: 6,5,4,3,2,1 = S1,S2,Z,S3,D,E
	m.activeFolderI = 1
	for i := range m.emails {
		m.emails[i].Folder = "ToScreen"
	}
	m.emails[2].From = "Z <z@example.com>" // uid 4
	m.emails[4].From = "D <d@example.com>" // uid 2
	m.emails[5].From = "E <e@example.com>" // uid 1
	m.applyFilter()
	m.inbox.Select(3) // uid 3 = S3
	res, _ := m.updateInbox(key("I"))
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 3 || got[0] != 4 || got[1] != 2 || got[2] != 1 {
		t.Fatalf("list = %v, want [4 2 1]", got)
	}
	if e := selectedEmail(mm.inbox); e == nil || e.UID != 2 {
		t.Errorf("cursor must land on D (uid 2), got %+v", e)
	}
}
