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
