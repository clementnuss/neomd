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

	// Attachment first, then the body tree: the big pdf is dropped, the body
	// tree and the small .ics are kept, in message order.
	plan := planLazy(&goimap.BodyStructureMultiPart{Subtype: "mixed", Children: []goimap.BodyStructure{pdf, alt, ics}})
	if plan == nil || fmt.Sprint(plan.kept) != "[[2] [3]]" {
		t.Fatalf("kept = %+v, want [[2] [3]]", plan)
	}
	if len(plan.dropped) != 1 || fmt.Sprint(plan.dropped[0].Part) != "[1]" || plan.dropped[0].Data != nil || plan.dropped[0].Filename != "a.pdf" || plan.dropped[0].Size != 3<<20 {
		t.Errorf("pdf must be dropped as metadata: %+v", plan.dropped)
	}
	// A big calendar part is dropped but still flagged as an invite.
	bigICS := *ics
	bigICS.Size = 400 << 10
	plan = planLazy(&goimap.BodyStructureMultiPart{Subtype: "mixed", Children: []goimap.BodyStructure{alt, &bigICS}})
	if plan == nil || len(plan.dropped) != 1 || !plan.dropped[0].IsCalendarInvite || plan.dropped[0].Filename != "i.ics" {
		t.Errorf("big .ics must be dropped as a calendar invite: %+v", plan)
	}
	// A big body root is still kept (it is the body).
	bigText := &goimap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: 2 << 20}
	plan = planLazy(&goimap.BodyStructureMultiPart{Subtype: "mixed", Children: []goimap.BodyStructure{bigText, pdf}})
	if plan == nil || fmt.Sprint(plan.kept) != "[[1]]" {
		t.Errorf("big body root must be kept: %+v", plan)
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

func TestLazy_ReassembleKeptKeepsTopHeadersAndParts(t *testing.T) {
	top := []byte("From: a@x\r\nSubject: s\r\nReferences: <r@x>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed;\r\n boundary=\"B\"\r\n\r\n")
	parts := []keptPart{
		{mime: []byte("Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n"), body: []byte("Hi =C3=A4\r\n")},
		{mime: []byte("Content-Type: text/plain; name=\"n.txt\"\r\nContent-Disposition: attachment; filename=\"n.txt\""), body: []byte("note")},
	}
	raw := reassembleKept(top, parts)
	s := string(raw)
	if strings.Contains(s, "boundary=\"B\"") || !strings.Contains(s, lazyBoundary) {
		t.Errorf("top Content-Type must be replaced by the synthetic boundary:\n%s", s)
	}
	for _, want := range []string{"From: a@x", "References: <r@x>", "Content-Transfer-Encoding: quoted-printable", "--" + lazyBoundary + "--\r\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	md, _, _, att, refs, _ := parseBody(raw)
	if !strings.Contains(md, "Hi ä") || refs != "<r@x>" {
		t.Errorf("parseBody on reassembled message: md=%q refs=%q", md, refs)
	}
	if len(att) != 1 || att[0].Filename != "n.txt" || string(att[0].Data) != "note" {
		t.Errorf("header without blank line must still parse: %+v", att)
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

// ── A3: keep every small top-level child, drop only the big ones ─────────

// mixedOf wraps parts (each starting with its own MIME header) into a
// multipart/mixed message with the usual top-level headers.
func mixedOf(parts ...string) []byte {
	var b strings.Builder
	b.WriteString("From: Sender <s@example.com>\r\nTo: u@example.com\r\nSubject: Shapes\r\n")
	b.WriteString("Message-ID: <shape@example.com>\r\nDate: Mon, 01 Sep 2026 10:00:00 +0000\r\n")
	b.WriteString("References: <root@example.com>\r\nMIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: multipart/mixed; boundary=\"B\"\r\n\r\n")
	for _, p := range parts {
		b.WriteString("--B\r\n")
		b.WriteString(p)
	}
	b.WriteString("--B--\r\n")
	return []byte(b.String())
}

// base64Part builds a base64 part of n random bytes with the given headers.
func base64Part(headers string, n int, seed int64) (string, []byte) {
	data := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(data)
	var b strings.Builder
	b.WriteString(headers)
	b.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	enc := base64.StdEncoding.EncodeToString(data)
	for i := 0; i < len(enc); i += 76 {
		j := i + 76
		if j > len(enc) {
			j = len(enc)
		}
		b.WriteString(enc[i:j])
		b.WriteString("\r\n")
	}
	return b.String(), data
}

func bigPDFPart() string {
	p, _ := base64Part("Content-Type: application/pdf; name=\"big.pdf\"\r\nContent-Disposition: attachment; filename=\"big.pdf\"\r\n", 1200*1024, 7)
	return p
}

type bodyResult struct {
	md, html, web, refs string
	att                 []Attachment
	spy                 SpyPixelInfo
}

// fullAndLazy seeds raw and returns the full fetch and the lazy fetch.
func fullAndLazy(t *testing.T, raw []byte) (full, lazy bodyResult) {
	t.Helper()
	cli, user := startMemIMAP(t)
	seedRaw(t, user, "INBOX", raw)
	ctx := context.Background()
	hdrs, err := cli.FetchHeaders(ctx, "INBOX", 10)
	if err != nil || len(hdrs) != 1 {
		t.Fatalf("headers: %v %v", hdrs, err)
	}
	e := hdrs[0]
	if e.Size < lazyBodyThreshold || planLazy(e.BodyStructure) == nil {
		t.Fatalf("precondition: size=%d plan=%v", e.Size, planLazy(e.BodyStructure))
	}
	full.md, full.html, full.web, full.att, full.refs, full.spy, err = cli.FetchBody(ctx, "INBOX", e.UID)
	if err != nil {
		t.Fatal(err)
	}
	lazy.md, lazy.html, lazy.web, lazy.att, lazy.refs, lazy.spy, err = cli.FetchBodyOf(ctx, "INBOX", e.UID, e.Size, e.BodyStructure)
	if err != nil {
		t.Fatal(err)
	}
	return full, lazy
}

// sameBodyExceptBig asserts lazy == full for text, HTML, references, spy
// pixels and attachment metadata/data; big.pdf is the one part left on the
// server (Data nil, Part set).
func sameBodyExceptBig(t *testing.T, full, lazy bodyResult) {
	t.Helper()
	if lazy.md != full.md {
		t.Errorf("markdown differs:\nlazy=%q\nfull=%q", lazy.md, full.md)
	}
	if lazy.html != full.html {
		t.Errorf("rawHTML differs:\nlazy=%q\nfull=%q", lazy.html, full.html)
	}
	if lazy.refs != full.refs || lazy.web != full.web {
		t.Errorf("refs/web differ: lazy=%q/%q full=%q/%q", lazy.refs, lazy.web, full.refs, full.web)
	}
	if fmt.Sprint(lazy.spy) != fmt.Sprint(full.spy) {
		t.Errorf("spy pixels differ: lazy=%+v full=%+v", lazy.spy, full.spy)
	}
	if len(lazy.att) != len(full.att) {
		t.Fatalf("attachment count: lazy=%d full=%d", len(lazy.att), len(full.att))
	}
	for i := range full.att {
		f, l := full.att[i], lazy.att[i]
		if l.Filename != f.Filename || l.ContentType != f.ContentType || l.ContentID != f.ContentID || l.IsCalendarInvite != f.IsCalendarInvite {
			t.Errorf("attachment %d metadata: lazy=%q %q cid=%q full=%q %q cid=%q", i, l.Filename, l.ContentType, l.ContentID, f.Filename, f.ContentType, f.ContentID)
		}
		if f.Filename == "big.pdf" {
			if l.Data != nil || len(l.Part) == 0 || l.Size == 0 {
				t.Errorf("big.pdf must stay on the server: data=%d part=%v size=%d", len(l.Data), l.Part, l.Size)
			}
			continue
		}
		if !bytes.Equal(l.Data, f.Data) || l.Data == nil {
			t.Errorf("attachment %d (%q) data differs: lazy=%d bytes full=%d bytes", i, f.Filename, len(l.Data), len(f.Data))
		}
	}
}

// (a) plain + HTML as direct mixed siblings: the HTML part (with its spy
// pixel) must be rendered exactly as on the full path.
func TestMem_FetchBodyOf_PlainAndHTMLSiblings(t *testing.T) {
	raw := mixedOf(
		"Content-Type: text/plain; charset=utf-8\r\n\r\nPlain version\r\n",
		"Content-Type: text/html; charset=utf-8\r\n\r\n<p>HTML <a href=\"https://example.com/v\">version</a></p><img src=\"https://track.example.com/o.gif\" width=\"1\" height=\"1\" alt=\"\">\r\n",
		bigPDFPart(),
	)
	full, lazy := fullAndLazy(t, raw)
	if full.spy.Count == 0 || !strings.Contains(full.md, "HTML") {
		t.Fatalf("precondition: full fetch renders HTML with a spy pixel: md=%q spy=%+v", full.md, full.spy)
	}
	sameBodyExceptBig(t, full, lazy)
}

// (b) an inline cid image directly under the mixed container keeps its
// placeholder and data.
func TestMem_FetchBodyOf_InlineCIDSibling(t *testing.T) {
	img, imgData := base64Part("Content-Type: image/png; name=\"logo.png\"\r\nContent-ID: <logo1>\r\nContent-Disposition: inline; filename=\"logo.png\"\r\n", 50*1024, 3)
	raw := mixedOf(
		"Content-Type: multipart/alternative; boundary=\"A\"\r\n\r\n"+
			"--A\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nHello Bye\r\n"+
			"--A\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Hello <img src=\"cid:logo1\"> Bye</p>\r\n"+
			"--A--\r\n",
		img,
		bigPDFPart(),
	)
	full, lazy := fullAndLazy(t, raw)
	sameBodyExceptBig(t, full, lazy)
	var found bool
	for _, a := range lazy.att {
		if a.ContentID == "logo1" {
			found = true
			if !bytes.Equal(a.Data, imgData) {
				t.Errorf("inline image must carry its data, got %d bytes", len(a.Data))
			}
		}
	}
	if !found {
		t.Error("inline image missing from lazy attachments")
	}
}

// (c) an inline text/calendar first child must not swallow the real body.
func TestMem_FetchBodyOf_CalendarFirstChild(t *testing.T) {
	ics := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:ev2\r\nSUMMARY:Planning\r\nDTSTART:20260901T100000Z\r\nDTEND:20260901T110000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	raw := mixedOf(
		"Content-Type: text/calendar; charset=utf-8; method=REQUEST\r\n\r\n"+ics+strings.Repeat("X-PAD:"+strings.Repeat("p", 60)+"\r\n", 25),
		"Content-Type: text/plain; charset=utf-8\r\n\r\nInvite body text\r\n",
		bigPDFPart(),
	)
	full, lazy := fullAndLazy(t, raw)
	if !strings.Contains(full.md, "Invite body text") {
		t.Fatalf("precondition: full md=%q", full.md)
	}
	sameBodyExceptBig(t, full, lazy)
}

// (d) nothing to drop → no lazy plan; the full fetch runs unchanged.
func TestLazy_PlanNilWhenEveryChildIsSmall(t *testing.T) {
	alt := &goimap.BodyStructureMultiPart{Subtype: "alternative", Children: []goimap.BodyStructure{
		&goimap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Size: 2000},
		&goimap.BodyStructureSinglePart{Type: "text", Subtype: "html", Size: 9000},
	}}
	small := &goimap.BodyStructureSinglePart{Type: "application", Subtype: "pdf", Size: 200 << 10,
		Extended: &goimap.BodyStructureSinglePartExt{Disposition: &goimap.BodyStructureDisposition{Value: "attachment", Params: map[string]string{"filename": "s.pdf"}}}}
	if p := planLazy(&goimap.BodyStructureMultiPart{Subtype: "mixed", Children: []goimap.BodyStructure{alt, small, small}}); p != nil {
		t.Errorf("all children small: plan must be nil, got %+v", p)
	}
}

// A kept part whose body carries our synthetic delimiter would split wrongly;
// the lazy path must fall back to the full fetch and render identically.
func TestMem_FetchBodyOf_BoundaryCollisionFallsBack(t *testing.T) {
	cli, user := startMemIMAP(t)
	raw, _ := bigMixedMessage(1200 * 1024)
	raw = bytes.Replace(raw, []byte("Hello =C3=A4 world"), []byte("line\r\n--"+lazyBoundary+"\r\nHello =C3=A4 world"), 1)
	seedRaw(t, user, "INBOX", raw)
	ctx := context.Background()
	hdrs, _ := cli.FetchHeaders(ctx, "INBOX", 10)
	e := hdrs[0]
	fullMD, _, _, fullAtt, _, _, err := cli.FetchBody(ctx, "INBOX", e.UID)
	if err != nil {
		t.Fatal(err)
	}
	lazyMD, _, _, lazyAtt, _, _, err := cli.FetchBodyOf(ctx, "INBOX", e.UID, e.Size, e.BodyStructure)
	if err != nil {
		t.Fatal(err)
	}
	if lazyMD != fullMD || len(lazyAtt) != len(fullAtt) || lazyAtt[len(lazyAtt)-1].Data == nil {
		t.Errorf("collision must fall back to the full fetch: md equal=%v att lazy=%d full=%d lastData=%d",
			lazyMD == fullMD, len(lazyAtt), len(fullAtt), len(lazyAtt[len(lazyAtt)-1].Data))
	}
}
