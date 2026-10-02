package ui

// Large mail leaves big attachments on the server (imap.Attachment.Data nil,
// Part set). The reader must download one on demand before opening it, and
// show that it is doing so.

import (
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
	res, cmd = mm.Update(attachmentFetchedMsg{idx: 0, data: []byte("%PDF-1.7"), then: "open"})
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
	res, _ := m.Update(attachmentFetchedMsg{idx: 0, err: testErr("FETCH NO"), then: "open"})
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
	res, cmd := m.Update(attachmentFetchedMsg{idx: 0, data: []byte("x"), then: "open"})
	mm := res.(Model)
	if cmd != nil || mm.isError {
		t.Error("a late download for an email no longer open must do nothing")
	}
}
