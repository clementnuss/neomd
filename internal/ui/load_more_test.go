package ui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sspaeti/neomd/internal/config"
	"github.com/sspaeti/neomd/internal/imap"
	"github.com/sspaeti/neomd/internal/merge"
)

// loadMoreModel: Archive tab with inbox_count = 3 and exactly 3 rows loaded,
// i.e. a folder that may hold more than the window.
func loadMoreModel(t *testing.T) Model {
	t.Helper()
	s, _ := merge.Load(filepath.Join(t.TempDir(), "merges.toml"))
	m := Model{merges: s, markedUIDs: map[uint32]bool{}, spyPixelKeys: map[string]bool{}, sortField: "date", sortReverse: true}
	m.cfg = &config.Config{}
	m.cfg.UI.InboxCount = 3
	m.cfg.Folders.Archive = "Archive"
	m.folders = []string{"Archive"}
	m.inbox = newInboxList(100, 10, "", "")
	now := time.Now()
	m.emails = []imap.Email{
		{UID: 30, Folder: "Archive", MessageID: "<30>", Subject: "c", From: "c@x", Date: now, Seen: true},
		{UID: 20, Folder: "Archive", MessageID: "<20>", Subject: "b", From: "b@x", Date: now.Add(-time.Hour), Seen: true},
		{UID: 10, Folder: "Archive", MessageID: "<10>", Subject: "a", From: "a@x", Date: now.Add(-2 * time.Hour), Seen: true},
	}
	m.applyFilter()
	return m
}

func TestLoadMore_JOnLastRowFetchesNextPage(t *testing.T) {
	m := loadMoreModel(t)
	m.inbox.Select(len(m.inbox.Items()) - 1)
	res, cmd := m.updateInbox(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	got := res.(Model)
	if !got.loadingMore || cmd == nil {
		t.Fatalf("loadingMore=%v cmd=%v; want a load-more fetch on j at the last row", got.loadingMore, cmd != nil)
	}
	if got.status != "Loading more…" {
		t.Errorf("status = %q", got.status)
	}
}

func TestLoadMore_CtrlDLandingOnLastRowFetchesNextPage(t *testing.T) {
	m := loadMoreModel(t)
	m.height = 5 // page step 10 clamps to the last row
	m.inbox.Select(0)
	res, cmd := m.updateInbox(tea.KeyMsg{Type: tea.KeyCtrlD})
	got := res.(Model)
	if !got.loadingMore || cmd == nil {
		t.Fatalf("loadingMore=%v cmd=%v; want a load-more fetch when ctrl+d lands on the last row", got.loadingMore, cmd != nil)
	}
}

func TestLoadMore_JAboveLastRowDoesNotFetch(t *testing.T) {
	m := loadMoreModel(t)
	m.inbox.Select(0)
	res, _ := m.updateInbox(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if res.(Model).loadingMore {
		t.Fatal("j in the middle of the list must not load more")
	}
}

func TestLoadMore_GOnLastRowDoesNotFetch(t *testing.T) {
	m := loadMoreModel(t)
	m.inbox.Select(0)
	res, _ := m.updateInbox(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	if res.(Model).loadingMore {
		t.Fatal("G jumps to the bottom but must not page through the whole folder")
	}
}

func TestLoadMore_CompleteFolderDoesNotFetch(t *testing.T) {
	m := loadMoreModel(t)
	m.folderComplete = map[string]bool{cacheKey("", "Archive"): true}
	m.inbox.Select(len(m.inbox.Items()) - 1)
	res, _ := m.updateInbox(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if res.(Model).loadingMore {
		t.Fatal("a folder known to be fully loaded must not fetch again")
	}
}

func TestLoadMore_UnlimitedInboxCountDoesNotFetch(t *testing.T) {
	m := loadMoreModel(t)
	m.cfg.UI.InboxCount = 0 // fetch-all: nothing more to load, ever
	m.inbox.Select(len(m.inbox.Items()) - 1)
	res, _ := m.updateInbox(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	if res.(Model).loadingMore {
		t.Fatal("inbox_count = 0 loads everything up front; no load-more")
	}
}

func TestLoadMore_ResultAppendsDedupesSortsAndKeepsCursor(t *testing.T) {
	m := loadMoreModel(t)
	m.loadingMore = true
	m.inbox.Select(2) // cursor on uid 10
	now := time.Now()
	res, _ := m.Update(moreEmailsLoadedMsg{folder: "Archive", emails: []imap.Email{
		{UID: 10, Folder: "Archive", Subject: "a", From: "a@x", Date: now.Add(-2 * time.Hour)}, // already loaded
		{UID: 5, Folder: "Archive", Subject: "old5", From: "e@x", Date: now.Add(-5 * time.Hour)},
		{UID: 4, Folder: "Archive", Subject: "old4", From: "d@x", Date: now.Add(-4 * time.Hour)},
		{UID: 3, Folder: "Archive", Subject: "old3", From: "c@x", Date: now.Add(-3 * time.Hour)},
	}})
	got := res.(Model)
	if got.loadingMore {
		t.Error("loadingMore still set")
	}
	if len(got.emails) != 6 {
		t.Fatalf("len(emails) = %d, want 6 (3 + 3 new, duplicate uid 10 dropped)", len(got.emails))
	}
	wantOrder := []uint32{30, 20, 10, 3, 4, 5}
	for i, e := range got.emails {
		if e.UID != wantOrder[i] {
			t.Fatalf("order = %v, want %v", uidsOfEmails(got.emails), wantOrder)
		}
	}
	if sel := selectedEmail(got.inbox); sel == nil || sel.UID != 10 {
		t.Errorf("cursor moved: %+v, want to stay on uid 10", sel)
	}
	if got.folderComplete[cacheKey("", "Archive")] {
		t.Error("a full page (3 of 3) must not mark the folder complete")
	}
	if snap := got.folderCache[cacheKey("", "Archive")]; len(snap.emails) != 6 {
		t.Errorf("folder cache not extended: %d rows", len(snap.emails))
	}
	if got.windowFor("Archive") != 6 {
		t.Errorf("windowFor = %d, want 6 so a refresh keeps the extended list", got.windowFor("Archive"))
	}
	if got.status != "Loaded 3 more · 6 emails" {
		t.Errorf("status = %q", got.status)
	}
}

func TestLoadMore_ShortPageMarksFolderComplete(t *testing.T) {
	m := loadMoreModel(t)
	m.loadingMore = true
	res, _ := m.Update(moreEmailsLoadedMsg{folder: "Archive", emails: []imap.Email{
		{UID: 3, Folder: "Archive", Subject: "old3", From: "c@x", Date: time.Now().Add(-3 * time.Hour)},
	}})
	got := res.(Model)
	if !got.folderComplete[cacheKey("", "Archive")] {
		t.Error("1 of 3 requested → folder is complete")
	}
	if got.status != "All 4 emails loaded" {
		t.Errorf("status = %q", got.status)
	}
}

func TestLoadMore_ResultForOtherFolderIsIgnored(t *testing.T) {
	m := loadMoreModel(t)
	m.loadingMore = true
	res, _ := m.Update(moreEmailsLoadedMsg{folder: "Sent", emails: []imap.Email{{UID: 99, Folder: "Sent"}}})
	got := res.(Model)
	if len(got.emails) != 3 || got.loadingMore {
		t.Fatalf("stale result applied: %d rows, loadingMore=%v", len(got.emails), got.loadingMore)
	}
}

func TestLoadMore_InitialLoadBelowWindowMarksComplete(t *testing.T) {
	m := loadMoreModel(t)
	m.loading = true
	res, _ := m.Update(emailsLoadedMsg{folder: "Archive", emails: []imap.Email{
		{UID: 1, Folder: "Archive", Subject: "only", From: "a@x", Date: time.Now()},
	}})
	got := res.(Model)
	if !got.folderComplete[cacheKey("", "Archive")] {
		t.Error("1 row for a window of 3 → the folder has nothing more; j at the bottom must not fetch")
	}
}

func TestLoadMore_WindowForDefaultsToInboxCount(t *testing.T) {
	m := loadMoreModel(t)
	if m.windowFor("Archive") != 3 {
		t.Errorf("windowFor = %d, want inbox_count 3", m.windowFor("Archive"))
	}
	m.cfg.UI.InboxCount = 0
	if m.windowFor("Archive") != 0 {
		t.Errorf("inbox_count 0 must stay 0 (fetch all)")
	}
}

func uidsOfEmails(es []imap.Email) []uint32 {
	out := make([]uint32, len(es))
	for i, e := range es {
		out[i] = e.UID
	}
	return out
}

func TestLoadMore_HintBarStartsWithMoreBelowUntilFolderComplete(t *testing.T) {
	m := loadMoreModel(t) // 3 rows loaded, window 3, completeness unknown → may have more
	got := m.inboxHintBar()
	cue, open := strings.Index(got, "more below"), strings.Index(got, "enter/l open")
	if cue < 0 || open < 0 || cue > open || !strings.Contains(got, "3 loaded") {
		t.Errorf("hint = %q; want the 'more below' cue before 'enter/l open' (the line is clipped at the terminal edge) and '3 loaded'", got)
	}
	m.folderComplete = map[string]bool{cacheKey("", "Archive"): true}
	if got := m.inboxHintBar(); strings.Contains(got, "more below") || !strings.Contains(got, "3 loaded") {
		t.Errorf("hint = %q; a complete folder shows the plain bar and count", got)
	}
}

func TestLoadMore_HintBarNoCueWhenInboxCountUnlimited(t *testing.T) {
	m := loadMoreModel(t)
	m.cfg.UI.InboxCount = 0
	if got := m.inboxHintBar(); strings.Contains(got, "more below") {
		t.Errorf("hint = %q; inbox_count 0 loads everything, no cue", got)
	}
}
