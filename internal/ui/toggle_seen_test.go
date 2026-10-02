package ui

// Optimistic read/unread toggle (n): the flag flips at once, the STORE runs
// behind the list, a fetch landing meanwhile cannot snap it back, and an
// error reverts the flag with the error in the status line.

import (
	"strings"
	"testing"

	"github.com/sspaeti/neomd/internal/imap"
)

func seenOf(m Model, uid uint32) bool {
	for _, e := range m.emails {
		if e.UID == uid {
			return e.Seen
		}
	}
	panic("uid not in list")
}

func TestToggleSeen_FlipsImmediatelyNoSpinner(t *testing.T) {
	m := instantModel(t, 3) // list 3,2,1, all seen, cursor on uid 3
	res, cmd := m.updateInbox(key("n"))
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("n must still fire the STORE command")
	}
	if mm.loading {
		t.Error("list must stay visible (loading=false)")
	}
	if seenOf(mm, 3) {
		t.Error("uid 3 must flip to unread immediately")
	}
	if !seenOf(mm, 2) || !seenOf(mm, 1) {
		t.Error("other rows must stay seen")
	}
	if mm.inbox.Index() != 1 {
		t.Errorf("cursor should move to the next row, index = %d", mm.inbox.Index())
	}
	if want, ok := mm.pendingSeen[removalKey{account: "P", folder: "INBOX", uid: 3}]; !ok || want {
		t.Errorf("pendingSeen must record wanted state false for uid 3, got ok=%v want=%v", ok, want)
	}
}

func TestToggleSeen_RefreshLandingMidFlightKeepsFlag(t *testing.T) {
	m := instantModel(t, 3)
	stale := append([]imap.Email(nil), m.emails...) // server still says seen
	res, _ := m.updateInbox(key("n"))
	mm := res.(Model)
	mm.refreshing = true
	res, _ = mm.Update(emailsLoadedMsg{emails: stale, folder: "INBOX", account: "P"})
	mm = res.(Model)
	if seenOf(mm, 3) {
		t.Error("a refresh landing while the STORE is in flight must not snap the flag back")
	}
	snap := mm.folderCache[cacheKey("P", "INBOX")]
	for _, e := range snap.emails {
		if e.UID == 3 && e.Seen {
			t.Error("cached snapshot must carry the pending flag too")
		}
	}
}

func TestToggleSeen_DoneClearsPendingAndKeepsFlag(t *testing.T) {
	m := instantModel(t, 3)
	res, _ := m.updateInbox(key("n"))
	mm := res.(Model)
	res, _ = mm.Update(toggleSeenDoneMsg{account: "P", ops: []seenOp{{folder: "INBOX", uid: 3, seen: false}}, done: 1})
	mm = res.(Model)
	if len(mm.pendingSeen) != 0 {
		t.Errorf("pendingSeen must be cleared, got %v", mm.pendingSeen)
	}
	if seenOf(mm, 3) {
		t.Error("flag stays flipped after success")
	}
	if mm.isError {
		t.Error("no error on success")
	}
}

func TestToggleSeen_ErrorRevertsAndShowsStatus(t *testing.T) {
	m := instantModel(t, 3)
	res, _ := m.updateInbox(key("n"))
	mm := res.(Model)
	res, _ = mm.Update(toggleSeenDoneMsg{account: "P", ops: []seenOp{{folder: "INBOX", uid: 3, seen: false}}, done: 0, err: testErr("STORE NO")})
	mm = res.(Model)
	if !seenOf(mm, 3) {
		t.Error("a failed STORE must revert the flag")
	}
	if !mm.isError || !strings.Contains(mm.status, "STORE NO") {
		t.Errorf("error must be visible in the status line, got %q", mm.status)
	}
	if len(mm.pendingSeen) != 0 {
		t.Errorf("pendingSeen must be cleared on error, got %v", mm.pendingSeen)
	}
}

func TestToggleSeen_BatchMarkedFlipsAllNoSpinner(t *testing.T) {
	m := instantModel(t, 3)
	m.markedUIDs[2] = true
	m.markedUIDs[1] = true
	res, cmd := m.updateInbox(key("n"))
	mm := res.(Model)
	if cmd == nil || mm.loading {
		t.Fatalf("batch n must fire the STOREs and keep the list: cmd=%v loading=%v", cmd != nil, mm.loading)
	}
	if seenOf(mm, 2) || seenOf(mm, 1) || !seenOf(mm, 3) {
		t.Errorf("marked rows flip, cursor row (unmarked) does not: 3=%v 2=%v 1=%v", seenOf(mm, 3), seenOf(mm, 2), seenOf(mm, 1))
	}
	if len(mm.markedUIDs) != 0 {
		t.Error("marks cleared")
	}
	if len(mm.pendingSeen) != 2 {
		t.Errorf("two pending flags, got %v", mm.pendingSeen)
	}
	// Partial failure: first STORE succeeded, second failed → only the second reverts.
	res, _ = mm.Update(toggleSeenDoneMsg{account: "P", ops: []seenOp{{folder: "INBOX", uid: 2, seen: false}, {folder: "INBOX", uid: 1, seen: false}}, done: 1, err: testErr("STORE NO")})
	mm = res.(Model)
	if seenOf(mm, 2) || !seenOf(mm, 1) {
		t.Errorf("after partial failure: uid 2 stays unread, uid 1 reverts; got 2=%v 1=%v", seenOf(mm, 2), seenOf(mm, 1))
	}
}
