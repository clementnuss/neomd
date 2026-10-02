package imap

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// bigMixedMessage builds a multipart/mixed message: an alternative text/html
// tree, a small .ics attachment and a large PDF (random bytes, base64).
// Returns the raw message and the PDF's original bytes.
func bigMixedMessage(pdfLen int) (raw, pdf []byte) {
	pdf = make([]byte, pdfLen)
	rnd := rand.New(rand.NewSource(1))
	rnd.Read(pdf)
	var b strings.Builder
	b.WriteString("From: Sender <s@example.com>\r\nTo: u@example.com\r\nSubject: Lazy\r\n")
	b.WriteString("Message-ID: <lazy@example.com>\r\nDate: Mon, 01 Sep 2026 10:00:00 +0000\r\n")
	b.WriteString("References: <root@example.com>\r\nMIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"B\"\r\n\r\n")
	b.WriteString("--B\r\nContent-Type: multipart/alternative; boundary=\"A\"\r\n\r\n")
	b.WriteString("--A\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nHello =C3=A4 world, see https://example.com/x\r\n")
	b.WriteString("--A\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Hello <b>&auml; world</b>, see <a href=\"https://example.com/x\">x</a></p>\r\n")
	b.WriteString("--A--\r\n")
	b.WriteString("--B\r\nContent-Type: text/calendar; name=\"invite.ics\"\r\nContent-Disposition: attachment; filename=\"invite.ics\"\r\n\r\n")
	b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:ev1\r\nSUMMARY:Lazy meeting\r\nDTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	b.WriteString("--B\r\nContent-Type: application/pdf; name=\"big.pdf\"\r\nContent-Disposition: attachment; filename=\"big.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString(pdf)
	for i := 0; i < len(enc); i += 76 {
		j := i + 76
		if j > len(enc) {
			j = len(enc)
		}
		b.WriteString(enc[i:j])
		b.WriteString("\r\n")
	}
	b.WriteString("--B--\r\n")
	return []byte(b.String()), pdf
}

func seedRaw(t *testing.T, user *imapmemserver.User, mailbox string, raw []byte) {
	t.Helper()
	if _, err := user.Append(mailbox, memLiteral{bytes.NewReader(raw), int64(len(raw))}, &goimap.AppendOptions{Time: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestLazy_PlanPicksBodyRootAndSiblings(t *testing.T) {
	att := &goimap.BodyStructureSinglePartExt{Disposition: &goimap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": "a.pdf"}}}
	alt := &goimap.BodyStructureMultiPart{Subtype: "alternative", Children: []goimap.BodyStructure{
		&goimap.BodyStructureSinglePart{Type: "text", Subtype: "plain"},
		&goimap.BodyStructureSinglePart{Type: "text", Subtype: "html"},
	}}
	pdf := &goimap.BodyStructureSinglePart{Type: "application", Subtype: "pdf", Size: 3 << 20, Extended: att}
	ics := &goimap.BodyStructureSinglePart{Type: "text", Subtype: "calendar", Size: 900, Params: map[string]string{"name": "i.ics"},
		Extended: &goimap.BodyStructureSinglePartExt{Disposition: &goimap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": "i.ics"}}}}

	// Attachment first, then the body tree: root must be child 2.
	plan := planLazy(&goimap.BodyStructureMultiPart{Subtype: "mixed", Children: []goimap.BodyStructure{pdf, alt, ics}})
	if plan == nil || fmt.Sprint(plan.root) != "[2]" {
		t.Fatalf("root = %v, want [2]", plan)
	}
	if len(plan.others) != 2 || fmt.Sprint(plan.others[0].att.Part) != "[1]" || plan.others[0].eager || plan.others[0].att.Filename != "a.pdf" || plan.others[0].att.Size != 3<<20 {
		t.Errorf("pdf sibling wrong: %+v", plan.others[0])
	}
	if !plan.others[1].eager || !plan.others[1].att.IsCalendarInvite || fmt.Sprint(plan.others[1].att.Part) != "[3]" {
		t.Errorf("ics sibling should be eager calendar: %+v", plan.others[1])
	}
	// Not mixed → full fetch.
	if planLazy(alt) != nil {
		t.Error("multipart/alternative must not be planned lazily")
	}
	if planLazy(&goimap.BodyStructureSinglePart{Type: "text", Subtype: "plain"}) != nil {
		t.Error("single part must not be planned lazily")
	}
	// Mixed with only attachments → no root → full fetch.
	if planLazy(&goimap.BodyStructureMultiPart{Subtype: "mixed", Children: []goimap.BodyStructure{pdf}}) != nil {
		t.Error("mixed without a body root must fall back")
	}
}

func TestLazy_ReassembleRootKeepsTopHeadersAndRootContentType(t *testing.T) {
	top := []byte("From: a@x\r\nSubject: s\r\nReferences: <r@x>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed;\r\n boundary=\"B\"\r\n\r\n")
	rootMIME := []byte("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n")
	body := []byte("Hi =C3=A4\r\n")
	raw := reassembleRoot(top, rootMIME, body)
	s := string(raw)
	if strings.Contains(s, "multipart/mixed") || strings.Contains(s, "boundary") {
		t.Errorf("top Content-Type must be replaced:\n%s", s)
	}
	for _, want := range []string{"From: a@x", "References: <r@x>", "Content-Type: text/plain; charset=utf-8", "Content-Transfer-Encoding: quoted-printable", "\r\n\r\nHi =C3=A4"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	md, _, _, _, refs, _ := parseBody(raw)
	if !strings.Contains(md, "Hi ä") || refs != "<r@x>" {
		t.Errorf("parseBody on reassembled message: md=%q refs=%q", md, refs)
	}
}

func TestMem_FetchBodyOf_LargeMixedSkipsBigAttachment(t *testing.T) {
	cli, user := startMemIMAP(t)
	raw, pdf := bigMixedMessage(1200 * 1024) // > lazyBodyThreshold once base64-encoded
	seedRaw(t, user, "INBOX", raw)
	ctx := context.Background()
	hdrs, err := cli.FetchHeaders(ctx, "INBOX", 10)
	if err != nil || len(hdrs) != 1 {
		t.Fatalf("headers: %v %v", hdrs, err)
	}
	e := hdrs[0]
	if e.BodyStructure == nil || e.Size < lazyBodyThreshold {
		t.Fatalf("precondition: size=%d bodystructure=%v", e.Size, e.BodyStructure != nil)
	}

	fullMD, fullHTML, _, fullAtt, fullRefs, _, err := cli.FetchBody(ctx, "INBOX", e.UID)
	if err != nil {
		t.Fatal(err)
	}
	lazyMD, lazyHTML, _, lazyAtt, lazyRefs, _, err := cli.FetchBodyOf(ctx, "INBOX", e.UID, e.Size, e.BodyStructure)
	if err != nil {
		t.Fatal(err)
	}
	if lazyMD != fullMD || lazyHTML != fullHTML || lazyRefs != fullRefs {
		t.Errorf("lazy text differs from full:\nlazy=%q\nfull=%q", lazyMD, fullMD)
	}
	if len(lazyAtt) != 2 || len(fullAtt) != 2 {
		t.Fatalf("attachments: lazy=%d full=%d", len(lazyAtt), len(fullAtt))
	}
	ics, big := lazyAtt[0], lazyAtt[1]
	if !ics.IsCalendarInvite || ics.Data == nil || !bytes.Contains(ics.Data, []byte("SUMMARY:Lazy meeting")) {
		t.Errorf("small .ics must be fetched eagerly with data: %+v", ics)
	}
	if big.Filename != "big.pdf" || big.Data != nil || fmt.Sprint(big.Part) != "[3]" || big.Size == 0 {
		t.Errorf("big pdf must be metadata only: name=%q data=%d part=%v size=%d", big.Filename, len(big.Data), big.Part, big.Size)
	}

	data, err := cli.FetchPart(ctx, "INBOX", e.UID, big.Part)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, pdf) {
		t.Errorf("FetchPart bytes differ: got %d bytes, want %d", len(data), len(pdf))
	}
	if !bytes.Equal(data, fullAtt[1].Data) {
		t.Error("FetchPart must equal what the full fetch decoded")
	}
}

func TestMem_FetchBodyOf_SmallMessageIsFullFetch(t *testing.T) {
	cli, user := startMemIMAP(t)
	raw, _ := bigMixedMessage(10 * 1024)
	seedRaw(t, user, "INBOX", raw)
	ctx := context.Background()
	hdrs, _ := cli.FetchHeaders(ctx, "INBOX", 10)
	e := hdrs[0]
	md1, html1, _, att1, _, _, err := cli.FetchBody(ctx, "INBOX", e.UID)
	if err != nil {
		t.Fatal(err)
	}
	md2, html2, _, att2, _, _, err := cli.FetchBodyOf(ctx, "INBOX", e.UID, e.Size, e.BodyStructure)
	if err != nil {
		t.Fatal(err)
	}
	if md1 != md2 || html1 != html2 || len(att1) != len(att2) {
		t.Fatalf("small message must take the identical full path")
	}
	for i := range att1 {
		if !bytes.Equal(att1[i].Data, att2[i].Data) || att2[i].Part != nil {
			t.Errorf("attachment %d must carry data and no Part on the full path", i)
		}
	}
}
