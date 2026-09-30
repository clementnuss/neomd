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

func keyTab() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyTab} }

func TestCache_SwitchToCachedFolderIsInstant(t *testing.T) {
	m := instantModel(t, 2)
	ts := mkEmail(50, "<ts@x>", "queued", "New <n@example.com>", 0, false)
	ts.Folder = "ToScreen"
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "ToScreen"): {emails: []imap.Email{ts}}}
	res, cmd := m.updateInbox(keyTab()) // Inbox -> ToScreen
	mm := res.(Model)
	if mm.loading {
		t.Error("cached folder must not show the spinner")
	}
	if !mm.refreshing || cmd == nil {
		t.Error("cached folder must still refresh in the background")
	}
	if got := uidsInList(mm); len(got) != 1 || got[0] != 50 {
		t.Errorf("list = %v, want cached [50]", got)
	}
}

func TestCache_SwitchToUncachedFolderShowsSpinner(t *testing.T) {
	m := instantModel(t, 2)
	res, cmd := m.updateInbox(keyTab())
	mm := res.(Model)
	if !mm.loading || cmd == nil {
		t.Error("uncached folder keeps today's spinner path")
	}
}

func TestCache_LoadedResultIsCachedAndShownOnlyForActiveFolder(t *testing.T) {
	m := instantModel(t, 1)
	other := mkEmail(60, "<o@x>", "late", "X <x@example.com>", 0, false)
	other.Folder = "Archive"
	res, _ := m.Update(emailsLoadedMsg{emails: []imap.Email{other}, folder: "Archive", account: "P"})
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 1 || got[0] != 1 {
		t.Errorf("late result for another folder must not replace the list, got %v", got)
	}
	if snap, ok := mm.folderCache[cacheKey("P", "Archive")]; !ok || len(snap.emails) != 1 {
		t.Error("late result must still be cached")
	}
	mine := mkEmail(2, "<n@x>", "new", "X <x@example.com>", 0, false)
	mine.Folder = "INBOX"
	mm.refreshing = true
	res, _ = mm.Update(emailsLoadedMsg{emails: []imap.Email{mine}, folder: "INBOX", account: "P"})
	mm = res.(Model)
	if got := uidsInList(mm); len(got) != 1 || got[0] != 2 || mm.refreshing {
		t.Errorf("active folder result must be shown and clear ↻: list=%v refreshing=%v", got, mm.refreshing)
	}
}

func TestCache_LateResultForOtherFolderIsCachedNotShown(t *testing.T) {
	// Same guard, exercised through a real switch: user leaves INBOX before
	// its fetch returns. The spinner path keeps the old rows behind the
	// spinner (today's behavior); the late INBOX row must not be added.
	m := instantModel(t, 1)
	res, _ := m.updateInbox(keyTab()) // now ToScreen, loading
	mm := res.(Model)
	stale := mkEmail(9, "<s@x>", "stale", "X <x@example.com>", 0, false)
	stale.Folder = "INBOX"
	res, _ = mm.Update(emailsLoadedMsg{emails: []imap.Email{stale}, folder: "INBOX", account: "P"})
	mm = res.(Model)
	if !mm.loading {
		t.Error("ToScreen is still loading; an INBOX result must not end the spinner")
	}
	for _, uid := range uidsInList(mm) {
		if uid == 9 {
			t.Errorf("INBOX rows must not appear under the ToScreen tab: %v", uidsInList(mm))
		}
	}
	if _, ok := mm.folderCache[cacheKey("P", "INBOX")]; !ok {
		t.Error("late INBOX result must still be cached")
	}
}

func TestCache_KeyedByAccount(t *testing.T) {
	m := instantModel(t, 1)
	m.cfg.Accounts = append(m.cfg.Accounts, config.AccountConfig{Name: "W", From: "w@x"})
	m.accounts = m.cfg.ActiveAccounts()
	m.clients = []*imap.Client{imap.New(imap.Config{}), imap.New(imap.Config{})}
	foreign := mkEmail(70, "<w@x>", "work", "X <x@example.com>", 0, false)
	foreign.Folder = "INBOX"
	res, _ := m.Update(emailsLoadedMsg{emails: []imap.Email{foreign}, folder: "INBOX", account: "W"})
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 1 || got[0] != 1 {
		t.Errorf("account W's INBOX must not show under account P, got %v", got)
	}
	if _, ok := mm.folderCache[cacheKey("W", "INBOX")]; !ok {
		t.Error("cached under account W")
	}
	if _, ok := mm.folderCache[cacheKey("P", "INBOX")]; ok {
		t.Error("must not be cached under account P")
	}
}

func TestCache_RefreshKeyBypassesCache(t *testing.T) {
	m := instantModel(t, 1)
	m.clients = []*imap.Client{imap.New(imap.Config{})} // R resets the mailbox selection on the client
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "INBOX"): {emails: m.emails}}
	res, cmd := m.updateInbox(key("R"))
	mm := res.(Model)
	if !mm.loading || cmd == nil {
		t.Error("R is the hard refresh: spinner path")
	}
}

func TestCache_DisabledByConfigUsesSpinner(t *testing.T) {
	m := instantModel(t, 2)
	f := false
	m.cfg.UI.InstantFolderSwitch = &f
	ts := mkEmail(50, "<ts@x>", "queued", "New <n@example.com>", 0, false)
	ts.Folder = "ToScreen"
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "ToScreen"): {emails: []imap.Email{ts}}}
	res, _ := m.updateInbox(keyTab())
	if !res.(Model).loading {
		t.Error("instant_folder_switch=false must restore the spinner path")
	}
}

func TestCache_HeaderShowsRefreshMarker(t *testing.T) {
	m := instantModel(t, 1)
	if strings.Contains(m.View(), "↻") {
		t.Error("no ↻ when not refreshing")
	}
	m.refreshing = true
	if !strings.Contains(m.View(), "↻") {
		t.Error("↻ expected while refreshing")
	}
}

// :go-spam must mark Spam as the active (off-tab) folder, otherwise the
// folder guard in emailsLoadedMsg drops its result and the spinner never ends.
func TestCache_GoSpamCommandResultIsShown(t *testing.T) {
	m := instantModel(t, 1)
	m = runCmd(t, m, "go-spam")
	sp := mkEmail(80, "<sp@x>", "spam", "X <x@example.com>", 0, false)
	sp.Folder = "Spam"
	res, _ := m.Update(emailsLoadedMsg{emails: []imap.Email{sp}, folder: "Spam", account: "P"})
	mm := res.(Model)
	if mm.loading {
		t.Error("Spam result must end the spinner")
	}
	if got := uidsInList(mm); len(got) != 1 || got[0] != 80 {
		t.Errorf("list = %v, want Spam [80]", got)
	}
}

func TestCache_LateRefreshDoesNotOverwriteSearchView(t *testing.T) {
	m := instantModel(t, 2)
	ts := mkEmail(50, "<ts@x>", "queued", "New <n@example.com>", 0, false)
	ts.Folder = "ToScreen"
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "ToScreen"): {emails: []imap.Email{ts}}}
	res, _ := m.updateInbox(keyTab()) // cache hit: ToScreen shown with ↻, refresh in flight
	mm := res.(Model)
	// User opens an IMAP search before the refresh lands.
	hit := mkEmail(90, "<hit@x>", "search hit", "X <x@example.com>", 0, false)
	hit.Folder = "Archive"
	mm.offTabFolder = "Search"
	mm.imapSearchResults = true
	mm.emails = []imap.Email{hit}
	mm.applyFilter()
	fresh := mkEmail(51, "<f@x>", "fresh", "New <n@example.com>", 0, false)
	fresh.Folder = "ToScreen"
	res, _ = mm.Update(emailsLoadedMsg{emails: []imap.Email{fresh}, folder: "ToScreen", account: "P"})
	mm = res.(Model)
	if got := uidsInList(mm); len(got) != 1 || got[0] != 90 {
		t.Errorf("search rows must remain, got %v", got)
	}
	if mm.offTabFolder != "Search" || !mm.imapSearchResults {
		t.Errorf("search view must stay: offTab=%q results=%v", mm.offTabFolder, mm.imapSearchResults)
	}
	if snap, ok := mm.folderCache[cacheKey("P", "ToScreen")]; !ok || len(snap.emails) != 1 || snap.emails[0].UID != 51 {
		t.Error("late refresh must still be cached under the tab folder")
	}
}

func TestCache_BgInboxFetchKeyedByFetchedAccount(t *testing.T) {
	m := instantModel(t, 1) // active account P
	w := mkEmail(70, "<w@x>", "work", "X <x@example.com>", 0, false)
	w.Folder = "INBOX"
	res, _ := m.Update(bgInboxFetchedMsg{emails: []imap.Email{w}, account: "W"})
	mm := res.(Model)
	if _, ok := mm.folderCache[cacheKey("W", "INBOX")]; !ok {
		t.Error("snapshot must land under the fetched account W")
	}
	if _, ok := mm.folderCache[cacheKey("P", "INBOX")]; ok {
		t.Error("snapshot must not land under the active account P")
	}
}

// R inside a synthetic view (Search) waits on the tab folder's fetch; the
// dropped result must still end the spinner, or the list stays hidden.
func TestCache_ReloadInSearchViewDoesNotHangSpinner(t *testing.T) {
	m := instantModel(t, 1)
	m.clients = []*imap.Client{imap.New(imap.Config{})}
	hit := mkEmail(90, "<hit@x>", "search hit", "X <x@example.com>", 0, false)
	hit.Folder = "Archive"
	m.offTabFolder = "Search"
	m.imapSearchResults = true
	m.emails = []imap.Email{hit}
	m.applyFilter()
	res, _ := m.updateInbox(key("R"))
	mm := res.(Model)
	res, _ = mm.Update(emailsLoadedMsg{emails: mm.emails[:0], folder: "INBOX", account: "P"})
	mm = res.(Model)
	if mm.loading || mm.refreshing {
		t.Errorf("spinner/↻ must end when the awaited fetch lands: loading=%v refreshing=%v", mm.loading, mm.refreshing)
	}
	if got := uidsInList(mm); len(got) != 1 || got[0] != 90 {
		t.Errorf("search rows must remain, got %v", got)
	}
}

func TestPrefetch_ListStartsWithToScreenAndSkipsCachedAndActive(t *testing.T) {
	m := instantModel(t, 1)
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "Archive"): {}}
	got := m.prefetchList("INBOX")
	if len(got) == 0 || got[0] != "ToScreen" {
		t.Fatalf("ToScreen must be first, got %v", got)
	}
	for _, f := range got {
		if f == "INBOX" || f == "Archive" {
			t.Errorf("active/cached folder %q must be skipped", f)
		}
	}
}

func TestPrefetch_MsgFillsCacheOnlyAndChains(t *testing.T) {
	m := instantModel(t, 1)
	ts := mkEmail(50, "<ts@x>", "queued", "New <n@example.com>", 0, false)
	ts.Folder = "ToScreen"
	res, cmd := m.Update(folderPrefetchedMsg{account: "P", folder: "ToScreen", emails: []imap.Email{ts}, remaining: []string{"Feed"}})
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 1 || got[0] != 1 {
		t.Errorf("prefetch must never touch the visible list, got %v", got)
	}
	if snap, ok := mm.folderCache[cacheKey("P", "ToScreen")]; !ok || len(snap.emails) != 1 {
		t.Error("prefetched folder not cached")
	}
	if cmd == nil {
		t.Error("remaining folders must be chained")
	}
	res, cmd = mm.Update(folderPrefetchedMsg{account: "P", folder: "Feed", emails: nil, remaining: nil})
	if cmd != nil {
		t.Error("chain ends when remaining is empty")
	}
}

func TestPrefetch_ScheduledOnceAfterFirstLoad(t *testing.T) {
	m := instantModel(t, 1)
	res, cmd := m.Update(emailsLoadedMsg{emails: m.emails, folder: "INBOX", account: "P"})
	mm := res.(Model)
	if !mm.prefetched || cmd == nil {
		t.Error("first load must schedule the prefetch")
	}
	f := false
	mm.cfg.UI.InstantFolderSwitch = &f
	mm.prefetched = false
	res, _ = mm.Update(emailsLoadedMsg{emails: m.emails, folder: "INBOX", account: "P"})
	if res.(Model).prefetched {
		t.Error("no prefetch when instant_folder_switch is off")
	}
}

func TestBgSync_SkipsAutoScreenWhenAccountChanged(t *testing.T) {
	m := instantModel(t, 1)
	if err := m.screener.Block("Sender <s@example.com>"); err != nil {
		t.Fatal(err)
	}
	m.bgSyncInProgress = true
	res, cmd := m.Update(bgInboxFetchedMsg{emails: m.emails, account: "W"})
	mm := res.(Model)
	if cmd != nil {
		t.Error("account changed mid-sync: no follow-up cmd must be scheduled")
	}
	if mm.bgSyncInProgress {
		t.Error("bgSyncInProgress must be cleared")
	}
	if snap, ok := mm.folderCache[cacheKey("W", "INBOX")]; !ok || len(snap.emails) != len(m.emails) {
		t.Error("snapshot must still be cached under the fetched account W")
	}
}
