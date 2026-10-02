package ui

// 0.10 pre-release audit fixes: screener guards, optimistic list edge cases,
// reply dot persistence. Each test pins one ruled fix.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sspaeti/neomd/internal/imap"
)

func asModel(res tea.Model) Model {
	if p, ok := res.(*Model); ok {
		return *p
	}
	return res.(Model)
}

// C1: :screen and S classify the loaded rows and MOVE them out of a folder;
// on any tab but the plain Inbox tab that would address another folder's
// UIDs. Both refuse.
func TestScreenCommand_RefusedOutsideInboxTab(t *testing.T) {
	block := func(m Model) Model {
		if err := m.screener.Block("Sender <s@example.com>"); err != nil {
			t.Fatal(err)
		}
		return m
	}
	toScreen := func() Model {
		m := block(instantModel(t, 3))
		m.activeFolderI = 1 // ToScreen tab
		for i := range m.emails {
			m.emails[i].Folder = "ToScreen"
		}
		m.applyFilter()
		return m
	}
	cases := map[string]Model{
		"ToScreen tab":             toScreen(),
		"Search view on Inbox tab": block(searchView(instantModel(t, 3))),
	}
	for name, m := range cases {
		// :screen
		mc := m
		res, _ := matchCmd("screen").run(&mc)
		mm := asModel(res)
		if len(mm.pendingMoves) != 0 || !strings.Contains(mm.status, "Inbox tab only") {
			t.Errorf("%s: :screen must refuse, got pending=%d status=%q", name, len(mm.pendingMoves), mm.status)
		}
		// S
		res, _ = m.updateInbox(key("S"))
		mm = asModel(res)
		if len(mm.pendingMoves) != 0 || !strings.Contains(mm.status, "Inbox tab only") {
			t.Errorf("%s: S must refuse, got pending=%d status=%q", name, len(mm.pendingMoves), mm.status)
		}
	}
	// On the Inbox tab it still plans.
	m := block(instantModel(t, 3))
	res, _ := matchCmd("screen").run(&m)
	if mm := asModel(res); len(mm.pendingMoves) != 3 {
		t.Errorf(":screen on the Inbox tab must plan the moves, status=%q", mm.status)
	}
}

// C1(b): the MOVE source is the email's own folder, never a hard-coded Inbox.
func TestAutoScreenPlan_UsesEmailFolderAsSource(t *testing.T) {
	emails := []imap.Email{
		mkEmail(10, "<a@x>", "a", "Zed <z@example.com>", 0, false),
		mkEmail(11, "<b@x>", "b", "Amy <a@example.com>", 0, false),
	}
	emails[0].Folder = "INBOX"
	emails[1].Folder = "ToScreen"
	plan := autoScreenPlan([]autoScreenMove{{email: &emails[0], dst: "Feed"}, {email: &emails[1], dst: "ScreenedOut"}})
	if plan[0].src != "INBOX" || plan[1].src != "ToScreen" {
		t.Errorf("plan sources = %q, %q; want each email's folder", plan[0].src, plan[1].src)
	}
}

// C2: the 5-minute background sync must honour the same guards as the Inbox
// load: empty lists pause screening, and auto_screen_on_load = false disables it.
func TestBgSync_SkipsScreeningWhenListsEmptyOrDisabled(t *testing.T) {
	// Empty lists (fresh install): everything would go to ToScreen.
	m := instantModel(t, 2)
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	m.bgSyncInProgress = true
	res, cmd := m.Update(bgInboxFetchedMsg{emails: inboxRows(2, 1), account: "P"})
	mm := res.(Model)
	if cmd != nil {
		t.Error("empty screener lists: bg sync must not MOVE anything")
	}
	if mm.bgSyncInProgress {
		t.Error("bg sync flag must be cleared when screening is skipped")
	}
	if snap, ok := mm.folderCache[cacheKey("P", "INBOX")]; !ok || len(snap.emails) != 2 {
		t.Error("the fetched Inbox is still cached")
	}

	// Lists filled but auto-screen disabled.
	m = instantModel(t, 2)
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	if err := m.screener.Block("Sender <s@example.com>"); err != nil {
		t.Fatal(err)
	}
	off := false
	m.cfg.UI.AutoScreenOnLoad = &off
	m.bgSyncInProgress = true
	res, cmd = m.Update(bgInboxFetchedMsg{emails: inboxRows(2, 1), account: "P"})
	if cmd != nil || res.(Model).bgSyncInProgress {
		t.Error("auto_screen_on_load = false: bg sync must not screen")
	}

	// Lists filled, enabled → bg sync screens as before.
	m.cfg.UI.AutoScreenOnLoad = nil
	m.bgSyncInProgress = true
	_, cmd = m.Update(bgInboxFetchedMsg{emails: inboxRows(2, 1), account: "P"})
	if cmd == nil {
		t.Error("with lists and auto-screen on, bg sync still screens")
	}
}

// B1: marks made while an optimistic x is in flight survive its completion.
func TestOptimistic_MarksMadeDuringMoveSurviveCompletion(t *testing.T) {
	m := instantModel(t, 5) // 5,4,3,2,1 cursor on 5
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	res, _ := m.updateInbox(key("x")) // removes 5, cursor on 4
	res, _ = res.(Model).updateInbox(key("m"))
	res, _ = res.(Model).updateInbox(key("m"))
	mm := res.(Model)
	if len(mm.markedUIDs) != 2 {
		t.Fatalf("setup: marks=%v", mm.markedUIDs)
	}
	res, _ = mm.Update(batchDoneMsg{removed: []removalKey{{account: "P", folder: "INBOX", uid: 5}}})
	mm = res.(Model)
	if !mm.markedUIDs[4] || !mm.markedUIDs[3] || len(mm.markedUIDs) != 2 {
		t.Errorf("marks made after x must survive its completion, got %v", mm.markedUIDs)
	}
}

// B2: U pressed while an optimistic move is still running must not pop the
// previous undo entry.
func TestOptimistic_UndoWaitsForInFlightMove(t *testing.T) {
	m := instantModel(t, 3)
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	older := []undoMove{{uid: 500, fromFolder: "Feed", toFolder: "Archive"}}
	m.undoStack = [][]undoMove{older}
	res, _ := m.updateInbox(key("x"))
	res, cmd := res.(Model).updateInbox(key("U"))
	mm := res.(Model)
	if cmd != nil || mm.loading {
		t.Error("U during an in-flight move must not start an undo")
	}
	if len(mm.undoStack) != 1 {
		t.Errorf("the older undo entry must stay, got %+v", mm.undoStack)
	}
	if !strings.Contains(mm.status, "still in progress") || mm.isError {
		t.Errorf("status should explain the wait, got %q (err=%v)", mm.status, mm.isError)
	}
	// After the move completes, U undoes it (the newest entry).
	res, _ = mm.Update(batchDoneMsg{removed: []removalKey{{account: "P", folder: "INBOX", uid: 3}},
		undo: []undoMove{{uid: 9, fromFolder: "INBOX", toFolder: "Trash"}}})
	mm = res.(Model)
	res, cmd = mm.updateInbox(key("U"))
	mm = res.(Model)
	if cmd == nil || len(mm.undoStack) != 1 || mm.undoStack[0][0].uid != 500 {
		t.Errorf("after completion U must undo the just-finished move, stack=%+v", mm.undoStack)
	}
}

// B3: a failing auto-screen MOVE reloads once with the error visible; that
// reload must not auto-screen again (no reload→screen→fail loop).
func TestAutoScreen_ErrorDoesNotLoop(t *testing.T) {
	m := instantModel(t, 2)
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	if err := m.screener.Block("Sender <s@example.com>"); err != nil {
		t.Fatal(err)
	}
	res, _ := m.Update(autoScreenDoneMsg{err: testErr("MOVE: NO [OVERQUOTA]"), moved: 0,
		removed: []removalKey{{account: "P", folder: "INBOX", uid: 2}}})
	mm := res.(Model)
	if !mm.loading {
		t.Fatal("setup: error must reload")
	}
	res, _ = mm.Update(emailsLoadedMsg{emails: inboxRows(2, 1), folder: "INBOX", account: "P"})
	mm = res.(Model)
	if got := uidsInList(mm); len(got) != 2 || mm.bulkProgress != nil || len(mm.pendingRemoval) != 0 {
		t.Errorf("the error reload must not auto-screen again: list=%v bulk=%v", got, mm.bulkProgress)
	}
	if !mm.isError || !strings.Contains(mm.status, "OVERQUOTA") {
		t.Errorf("error must stay visible, got %q", mm.status)
	}
	// The next load (R, tab switch, bg tick) retries normally.
	mm.loading = true
	res, _ = mm.Update(emailsLoadedMsg{emails: inboxRows(2, 1), folder: "INBOX", account: "P"})
	if got := uidsInList(res.(Model)); len(got) != 0 {
		t.Errorf("a later load must auto-screen again, list=%v", got)
	}
}

// B4: a screener key on a row already in its target folder writes the list
// but does not MOVE — the row must stay.
func TestOptimistic_ScreenInInboxKeepsRow(t *testing.T) {
	m := instantModel(t, 3)
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	res, cmd := m.updateInbox(key("I"))
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("I must still run (it writes the screened-in list)")
	}
	if got := uidsInList(mm); len(got) != 3 {
		t.Errorf("I on an Inbox row must keep it, list=%v", got)
	}
	if len(mm.pendingRemoval) != 0 {
		t.Errorf("nothing is moving, pendingRemoval=%v", mm.pendingRemoval)
	}
}

// B5: a ↻ refresh landing while a body fetch (or T, V, …) runs is a
// background result: marks and / filter stay, the other spinner keeps going.
func TestCache_RefreshLandingDuringBodyFetchKeepsMarksAndSpinner(t *testing.T) {
	m := instantModel(t, 3)
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	m.refreshing = true
	m.filterText = "s"
	m.markedUIDs[2] = true
	m.applyFilter()
	m.inbox.Select(0)
	res, _ := m.updateInbox(tea.KeyMsg{Type: tea.KeyEnter})
	mm := res.(Model)
	if !mm.loading {
		t.Fatal("setup: enter should set loading")
	}
	res, _ = mm.Update(emailsLoadedMsg{emails: inboxRows(3, 2, 1), folder: "INBOX", account: "P"})
	mm = res.(Model)
	if mm.filterText == "" || !mm.markedUIDs[2] {
		t.Errorf("background refresh wiped filter/marks: filter=%q marks=%v", mm.filterText, mm.markedUIDs)
	}
	if !mm.loading {
		t.Error("the body fetch's spinner must keep running")
	}
	if mm.refreshing {
		t.Error("↻ must end")
	}
	// A spinner reload (R) is never mistaken for a ↻ result.
	m = instantModel(t, 3)
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	m.refreshing = true
	res, _ = m.updateInbox(key("R"))
	if mm := res.(Model); mm.refreshing || !mm.loading {
		t.Errorf("R must be a pure spinner load: loading=%v refreshing=%v", mm.loading, mm.refreshing)
	}
}

// B6: the bg sync caches the Inbox WITHOUT the rows it is about to screen.
func TestCache_BgInboxSnapshotExcludesScreenedRows(t *testing.T) {
	m := instantModel(t, 1)
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	m.activeFolderI = 1 // on ToScreen
	if err := m.screener.Block("Sender <s@example.com>"); err != nil {
		t.Fatal(err)
	}
	if err := m.screener.Approve("Friend <f@example.com>"); err != nil {
		t.Fatal(err)
	}
	rows := inboxRows(2, 1)
	rows[1].From = "Friend <f@example.com>" // uid 1 stays in Inbox
	m.bgSyncInProgress = true
	_, cmd := m.Update(bgInboxFetchedMsg{emails: rows, account: "P"})
	res, _ := m.Update(bgInboxFetchedMsg{emails: rows, account: "P"})
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("setup: uid 2 must be screened out")
	}
	snap := mm.folderCache[cacheKey("P", "INBOX")]
	if len(snap.emails) != 1 || snap.emails[0].UID != 1 {
		var u []uint32
		for _, e := range snap.emails {
			u = append(u, e.UID)
		}
		t.Errorf("cached Inbox = %v, want only [1]", u)
	}
}

// B7: the header hint names the key that clears marks (ctrl+u; U is undo).
func TestInboxHeaderMarkHintNamesCtrlU(t *testing.T) {
	m := instantModel(t, 2)
	m.markedUIDs[1] = true
	v := m.viewInbox()
	if !strings.Contains(v, "ctrl+u to clear") || strings.Contains(v, "U to clear]") {
		t.Errorf("mark hint must say ctrl+u, got header:\n%s", v)
	}
}

// D1: the · reply dot set on sendDoneMsg must survive any local list
// rebuild (applyFilter, optimistic removal, cache-hit tab switch).
func TestReplyDotSurvivesListRebuild(t *testing.T) {
	m := instantModel(t, 3)
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "INBOX"): {emails: append([]imap.Email(nil), m.emails...)}}
	res, _ := m.Update(sendDoneMsg{replyToUID: 2, replyToFolder: "INBOX"})
	mm := res.(Model)
	answered := func(mm Model) bool {
		for _, it := range mm.inbox.Items() {
			if e := it.(emailItem).email; e.UID == 2 {
				return e.Answered
			}
		}
		return false
	}
	if !answered(mm) {
		t.Fatal("dot must show right after send")
	}
	mm.applyFilter()
	if !answered(mm) {
		t.Error("dot lost on list rebuild (m.emails not updated)")
	}
	for _, e := range mm.folderCache[cacheKey("P", "INBOX")].emails {
		if e.UID == 2 && !e.Answered {
			t.Error("dot lost in the folder cache snapshot")
		}
	}
	// Tab away and back: the cache-hit list keeps the dot.
	res, _ = mm.updateInbox(keyTab())
	res, _ = res.(Model).updateInbox(tea.KeyMsg{Type: tea.KeyShiftTab})
	if !answered(res.(Model)) {
		t.Error("dot lost on a cache-hit tab switch")
	}
}
