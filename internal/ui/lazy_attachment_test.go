package ui

// Large mail leaves big attachments on the server (imap.Attachment.Data nil,
// Part set). The reader must download one on demand before opening it, and
// show that it is doing so.

import (
	"os"
	"strings"
	"testing"

	"github.com/sspaeti/neomd/internal/imap"
)

func readerModelWithLazyAttachment(t *testing.T) Model {
	t.Helper()
	m := instantModel(t, 1)
	m.state = stateReading
	e := m.emails[0]
	m.openEmail = &e
	m.openAttachments = []imap.Attachment{
		{Filename: "big.pdf", ContentType: "application/pdf", Size: 2200 * 1024, Part: []int{3}},
		{Filename: "small.txt", ContentType: "text/plain", Data: []byte("hi")},
	}
	return m
}

func TestLazyAttachment_OpenDownloadsFirst(t *testing.T) {
	m := readerModelWithLazyAttachment(t)
	res, cmd := m.updateReader(key("1"))
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("1 on a server-side attachment must fire the download")
	}
	if !strings.Contains(mm.status, "Downloading big.pdf") || !strings.Contains(mm.status, "2.1 MB") {
		t.Errorf("status should say what is downloading, got %q", mm.status)
	}
	if mm.openAttachments[0].Data != nil {
		t.Error("data must not be invented before the download lands")
	}
	// Download lands → data stored, open continues (a non-nil cmd is the open step).
	res, cmd = mm.Update(attachmentFetchedMsg{folder: "INBOX", uid: 1, idx: 0, data: []byte("%PDF-1.7"), then: "open"})
	mm = res.(Model)
	if string(mm.openAttachments[0].Data) != "%PDF-1.7" {
		t.Error("downloaded bytes must be stored on the attachment")
	}
	if cmd == nil {
		t.Error("after the download the open action must run")
	}
	if mm.status != "" || mm.isError {
		t.Errorf("status cleared after download, got %q err=%v", mm.status, mm.isError)
	}
}

func TestLazyAttachment_OpenWithDataDoesNotDownload(t *testing.T) {
	m := readerModelWithLazyAttachment(t)
	res, cmd := m.updateReader(key("2"))
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("2 must open the attachment")
	}
	if strings.Contains(mm.status, "Downloading") {
		t.Errorf("an attachment with data opens directly, got status %q", mm.status)
	}
}

func TestLazyAttachment_DownloadErrorIsVisible(t *testing.T) {
	m := readerModelWithLazyAttachment(t)
	res, _ := m.Update(attachmentFetchedMsg{folder: "INBOX", uid: 1, idx: 0, err: testErr("FETCH NO"), then: "open"})
	mm := res.(Model)
	if !mm.isError || !strings.Contains(mm.status, "FETCH NO") {
		t.Errorf("download error must show in the status line, got %q", mm.status)
	}
	if mm.openAttachments[0].Data != nil {
		t.Error("no data on error")
	}
}

func TestLazyAttachment_ReaderHeaderShowsSize(t *testing.T) {
	m := readerModelWithLazyAttachment(t)
	hdr := renderEmailHeader(m.openEmail, m.openAttachments, imap.SpyPixelInfo{}, 120)
	if !strings.Contains(hdr, "[1] big.pdf (2.1 MB)") {
		t.Errorf("size expected next to the lazy attachment, got:\n%s", hdr)
	}
	if !strings.Contains(hdr, "[2] small.txt") || strings.Contains(hdr, "small.txt (") {
		t.Errorf("attachments without a known size keep the old label, got:\n%s", hdr)
	}
}

func TestLazyAttachment_StaleResultAfterReaderMovedOnIsIgnored(t *testing.T) {
	m := readerModelWithLazyAttachment(t)
	m.openAttachments = nil // user already left the email
	res, cmd := m.Update(attachmentFetchedMsg{folder: "INBOX", uid: 1, idx: 0, data: []byte("x"), then: "open"})
	mm := res.(Model)
	if cmd != nil || mm.isError {
		t.Error("a late download for an email no longer open must do nothing")
	}
}

// A download started on mail A must never land on mail B opened meanwhile:
// the result is matched by folder+UID, not by attachment index alone.
func TestLazyAttachment_StaleDownloadForOtherEmailIgnored(t *testing.T) {
	m := readerModelWithLazyAttachment(t) // mail A (INBOX uid 1)
	res, cmd := m.updateReader(key("1"))
	if cmd == nil {
		t.Fatal("setup: 1 must fire the download")
	}
	mm := res.(Model)
	// The user leaves A and opens B, which has its own attachment at index 0.
	b := mm.emails[0]
	b.UID = 999
	mm.openEmail = &b
	mm.openAttachments = []imap.Attachment{{Filename: "b-contract.pdf", ContentType: "application/pdf", Data: []byte("%PDF-B")}}
	// A's download lands.
	res, cmd = mm.Update(attachmentFetchedMsg{folder: "INBOX", uid: 1, idx: 0, data: []byte("%PDF-A"), then: "open"})
	mm = res.(Model)
	if cmd != nil {
		t.Error("a download for another email must not run its follow-up action")
	}
	if string(mm.openAttachments[0].Data) != "%PDF-B" {
		t.Errorf("B's attachment was overwritten with A's bytes: %q", mm.openAttachments[0].Data)
	}
}

// fetchAttachmentCmd tags the result with the email it was started on.
func TestLazyAttachment_FetchCmdCarriesFolderAndUID(t *testing.T) {
	m := readerModelWithLazyAttachment(t)
	msg := m.fetchAttachmentCmd(0, "open")() // no IMAP client → error msg, still tagged
	got, ok := msg.(attachmentFetchedMsg)
	if !ok {
		t.Fatalf("got %T", msg)
	}
	if got.folder != "INBOX" || got.uid != 1 {
		t.Errorf("result must carry folder+uid of the open email, got %q/%d", got.folder, got.uid)
	}
}

// E on a large draft must download every attachment the lazy fetch left on
// the server before writing the temp files — never attach a 0-byte file.
func TestLazyAttachment_ContinueDraftDownloadsFirst(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	m := readerModelWithLazyAttachment(t)
	m.compose = newComposeModel()
	m.openEmail.Folder = "Drafts"
	m.openBody = "draft body"
	res, cmd := m.updateReader(key("E"))
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("E with a server-side attachment must fire the download")
	}
	if mm.state == stateCompose || mm.attachments != nil {
		t.Fatalf("no temp file / editor before the download: state=%v attachments=%v", mm.state, mm.attachments)
	}
	if !strings.Contains(mm.status, "Downloading big.pdf") {
		t.Errorf("status should say what is downloading, got %q", mm.status)
	}
	if msg, ok := cmd().(attachmentFetchedMsg); !ok || msg.then != "draft" || msg.idx != 0 {
		t.Errorf("download must continue the draft, got %+v", msg)
	}
	// The download lands → continueDraft proceeds with the real bytes.
	res, cmd = mm.Update(attachmentFetchedMsg{folder: "Drafts", uid: 1, idx: 0, data: []byte("%PDF-1.7 full"), then: "draft"})
	mm = res.(Model)
	if cmd == nil || mm.state != stateCompose {
		t.Fatalf("after the download continueDraft must open the editor: state=%v cmd=%v", mm.state, cmd != nil)
	}
	if len(mm.attachments) != 2 {
		t.Fatalf("attachments = %v", mm.attachments)
	}
	data, err := os.ReadFile(mm.attachments[0])
	if err != nil || string(data) != "%PDF-1.7 full" {
		t.Errorf("big.pdf temp file = %q (%v), want the downloaded bytes", data, err)
	}
}

func TestWriteAttachmentsTemp_RefusesServerSideAttachment(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	_, err := writeAttachmentsTemp([]imap.Attachment{
		{Filename: "ok.txt", Data: []byte("x")},
		{Filename: "big.pdf", Part: []int{2}},
	})
	if err == nil || !strings.Contains(err.Error(), "big.pdf") {
		t.Errorf("an attachment still on the server must be refused, got %v", err)
	}
}
