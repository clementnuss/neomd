package imap

// Lazy body fetch for large messages.
//
// Opening a 6 MB mail used to FETCH BODY.PEEK[] — the whole raw message,
// attachments included — and parse all of it before the reader could draw.
// The reader only needs the text parts; attachments are needed when the user
// presses 1–9, forwards, opens an .ics or renders inline images.
//
// For a multipart/mixed message at or above lazyBodyThreshold, FetchBodyOf
// keeps every top-level child except the big ones: the body root (the first
// non-attachment text/multipart child) and every child of at most
// lazyPartEagerMax come in ONE FETCH and are reassembled into a synthetic
// multipart/mixed message, so the existing parseBody sees the original
// structure minus the dropped parts — plain+HTML siblings, inline cid images,
// calendar parts and attachment names behave exactly as on the full fetch.
// Big children become Attachment entries with metadata only (Data == nil,
// Part set); FetchPart downloads one on demand.
//
// Everything else (small mail, non-mixed structure, no BODYSTRUCTURE) takes
// the full-fetch path exactly as before.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/textproto"
)

const (
	// lazyBodyThreshold is the RFC822 size from which the reader fetches text
	// parts only and leaves attachments on the server until asked for.
	lazyBodyThreshold = 1 << 20 // 1 MB
	// lazyPartEagerMax: sibling parts up to this size are still fetched with
	// the body (calendar invites, small images), so the reader behaves as before.
	lazyPartEagerMax = 256 << 10 // 256 KB
)

// lazyPlan splits a multipart/mixed message's top-level children into the
// ones fetched with the body and the big ones left on the server.
type lazyPlan struct {
	kept    [][]int      // section paths fetched with the body, in message order
	dropped []Attachment // metadata only (Data nil, Part set)
}

// planLazy returns nil — callers then take the full fetch — when bs is not
// a multipart/mixed container with an identifiable body root, or when no
// child is big enough to be worth leaving on the server.
func planLazy(bs imap.BodyStructure) *lazyPlan {
	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok || !strings.EqualFold(mp.Subtype, "mixed") {
		return nil
	}
	plan := &lazyPlan{}
	rootSeen := false
	for i, child := range mp.Children {
		path := []int{i + 1}
		isRoot := !rootSeen && isBodyRoot(child)
		rootSeen = rootSeen || isRoot
		if isRoot || subtreeSize(child) <= lazyPartEagerMax {
			plan.kept = append(plan.kept, path)
			continue
		}
		plan.dropped = append(plan.dropped, droppedAttachment(path, child))
	}
	if !rootSeen || len(plan.dropped) == 0 {
		return nil
	}
	return plan
}

func isAttachmentDisposition(d *imap.BodyStructureDisposition) bool {
	return d != nil && strings.EqualFold(d.Value, "attachment")
}

func isBodyRoot(bs imap.BodyStructure) bool {
	switch p := bs.(type) {
	case *imap.BodyStructureSinglePart:
		if isAttachmentDisposition(p.Disposition()) {
			return false
		}
		return strings.EqualFold(p.Type, "text")
	case *imap.BodyStructureMultiPart:
		return !isAttachmentDisposition(p.Disposition())
	}
	return false
}

// subtreeSize is the summed transfer size of every single part in bs.
func subtreeSize(bs imap.BodyStructure) uint32 {
	switch p := bs.(type) {
	case *imap.BodyStructureSinglePart:
		return p.Size
	case *imap.BodyStructureMultiPart:
		var n uint32
		for _, c := range p.Children {
			n += subtreeSize(c)
		}
		return n
	}
	return 0
}

// droppedAttachment is the metadata-only Attachment for a child left on the
// server.
func droppedAttachment(path []int, bs imap.BodyStructure) Attachment {
	att := Attachment{Size: subtreeSize(bs), Part: path}
	switch p := bs.(type) {
	case *imap.BodyStructureSinglePart:
		att.ContentType = strings.ToLower(p.MediaType())
		att.Filename = p.Filename()
		att.ContentID = strings.Trim(p.ID, "<>")
	case *imap.BodyStructureMultiPart:
		att.ContentType = strings.ToLower(p.MediaType())
		if d := p.Disposition(); d != nil {
			att.Filename = d.Params["filename"]
		}
	}
	att.IsCalendarInvite = isCalendarPart(att.ContentType, att.Filename)
	return att
}

// sectionKey identifies a fetched section regardless of pointer identity.
func sectionKey(s *imap.FetchItemBodySection) string {
	return fmt.Sprintf("%s/%v", s.Specifier, s.Part)
}

// lazyBoundary separates the kept children in the synthetic message.
const lazyBoundary = "=_neomd_lazy_"

// keptPart is one fetched top-level child: its MIME header and raw body.
type keptPart struct {
	mime, body []byte
}

// reassembleKept builds a synthetic multipart/mixed message from the
// top-level header and the kept children: the top header keeps
// From/To/Subject/Date/List-*/References… while its Content-* fields are
// replaced by a multipart/mixed type with lazyBoundary, so parseBody sees
// the original structure minus the dropped parts.
func reassembleKept(top []byte, parts []keptPart) []byte {
	th := readHeaderLenient(top)
	for _, k := range []string{"Content-Type", "Content-Transfer-Encoding", "Content-Disposition", "Content-Id", "Content-Description", "Content-Location"} {
		th.Del(k)
	}
	th.Set("Content-Type", `multipart/mixed; boundary="`+lazyBoundary+`"`)
	if !th.Has("Mime-Version") {
		th.Set("MIME-Version", "1.0")
	}
	var buf bytes.Buffer
	_ = textproto.WriteHeader(&buf, th)
	for _, p := range parts {
		buf.WriteString("--" + lazyBoundary + "\r\n")
		buf.Write(p.mime)
		switch {
		case bytes.HasSuffix(p.mime, []byte("\r\n\r\n")), bytes.HasSuffix(p.mime, []byte("\n\n")):
		case bytes.HasSuffix(p.mime, []byte("\r\n")), bytes.HasSuffix(p.mime, []byte("\n")):
			buf.WriteString("\r\n")
		default:
			buf.WriteString("\r\n\r\n")
		}
		buf.Write(p.body)
		// The CRLF before a delimiter belongs to the delimiter, not to the
		// part: BODY[n] ends where that CRLF starts, so always add one.
		buf.WriteString("\r\n")
	}
	buf.WriteString("--" + lazyBoundary + "--\r\n")
	return buf.Bytes()
}

// readHeaderLenient parses a header block that may or may not end with the
// empty line; a parse error yields whatever was read so far.
func readHeaderLenient(raw []byte) textproto.Header {
	if !bytes.HasSuffix(raw, []byte("\r\n\r\n")) && !bytes.HasSuffix(raw, []byte("\n\n")) {
		raw = append(append([]byte(nil), raw...), '\r', '\n')
	}
	h, _ := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	return h
}

// decodePart turns a part's MIME header + transfer-encoded body into the
// decoded bytes (base64 / quoted-printable undone). Unknown encodings fall
// back to the raw body rather than failing.
func decodePart(mimeHdr, body []byte) []byte {
	raw := append(append([]byte(nil), mimeHdr...), body...)
	if !bytes.HasSuffix(mimeHdr, []byte("\r\n\r\n")) && !bytes.HasSuffix(mimeHdr, []byte("\n\n")) {
		raw = append(append(append([]byte(nil), mimeHdr...), '\r', '\n'), body...)
	}
	e, err := message.Read(bytes.NewReader(raw))
	if err != nil && !message.IsUnknownCharset(err) && !message.IsUnknownEncoding(err) {
		return body
	}
	data, err := io.ReadAll(e.Body)
	if err != nil {
		return body
	}
	return data
}

// FetchBodyOf is FetchBody with the knowledge FetchHeaders already has: the
// message size and its BODYSTRUCTURE. Small or non-mixed messages take the
// full fetch; large multipart/mixed messages fetch the text tree only and
// return big attachments as metadata (Data nil, Part set) for FetchPart.
func (c *Client) FetchBodyOf(ctx context.Context, folder string, uid uint32, size uint32, bs imap.BodyStructure) (string, string, string, []Attachment, string, SpyPixelInfo, error) {
	plan := planLazy(bs)
	if size < lazyBodyThreshold || plan == nil {
		return c.FetchBody(ctx, folder, uid)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	defer trace(time.Now(), "FetchBodyOf(lazy) %s uid=%d size=%d", folder, uid, size)
	var markdown, rawHTML, webURL, references string
	var attachments []Attachment
	var spyPixels SpyPixelInfo
	incomplete := false
	err := c.withConnRetry(ctx, func(conn *imapclient.Client) error {
		attachments, incomplete = nil, false // reset on retry
		if err := c.selectMailbox(folder); err != nil {
			return err
		}
		sections := []*imap.FetchItemBodySection{{Specifier: imap.PartSpecifierHeader, Peek: true}}
		for _, path := range plan.kept {
			sections = append(sections,
				&imap.FetchItemBodySection{Part: path, Specifier: imap.PartSpecifierMIME, Peek: true},
				&imap.FetchItemBodySection{Part: path, Peek: true})
		}
		var fetchSet imap.UIDSet
		fetchSet.AddNum(imap.UID(uid))
		msgs, err := conn.Fetch(fetchSet, &imap.FetchOptions{UID: true, BodySection: sections}).Collect()
		if err != nil {
			return fmt.Errorf("FETCH body parts uid=%d: %w", uid, err)
		}
		if len(msgs) == 0 {
			return fmt.Errorf("message uid=%d not found in %s", uid, folder)
		}
		got := make(map[string][]byte, len(msgs[0].BodySection))
		for _, s := range msgs[0].BodySection {
			got[sectionKey(s.Section)] = s.Bytes
		}
		top, ok := got[sectionKey(sections[0])]
		if !ok || len(top) == 0 {
			incomplete = true
			return nil
		}
		parts := make([]keptPart, 0, len(plan.kept))
		for _, path := range plan.kept {
			mime, okM := got[sectionKey(&imap.FetchItemBodySection{Part: path, Specifier: imap.PartSpecifierMIME})]
			body, okB := got[sectionKey(&imap.FetchItemBodySection{Part: path})]
			if !okM || !okB {
				// A server that does not echo a section exactly as asked:
				// never guess — take the full fetch below.
				incomplete = true
				return nil
			}
			if bytes.Contains(mime, []byte("--"+lazyBoundary)) || bytes.Contains(body, []byte("--"+lazyBoundary)) {
				// A kept part that itself carries our delimiter would split
				// wrongly in the synthetic message — take the full fetch.
				incomplete = true
				return nil
			}
			parts = append(parts, keptPart{mime: mime, body: body})
		}
		markdown, rawHTML, webURL, attachments, references, spyPixels = parseBody(reassembleKept(top, parts))
		for _, att := range plan.dropped {
			att.Part = append([]int(nil), att.Part...)
			attachments = append(attachments, att)
		}
		return nil
	})
	if err == nil && incomplete {
		return c.FetchBody(ctx, folder, uid)
	}
	return markdown, rawHTML, webURL, attachments, references, spyPixels, err
}

// FetchPart downloads and decodes one MIME part (section path from
// Attachment.Part) — the on-demand half of the lazy body fetch.
func (c *Client) FetchPart(ctx context.Context, folder string, uid uint32, part []int) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	defer trace(time.Now(), "FetchPart %s uid=%d part=%v", folder, uid, part)
	if len(part) == 0 {
		return nil, fmt.Errorf("FetchPart: empty section path")
	}
	var data []byte
	err := c.withConnRetry(ctx, func(conn *imapclient.Client) error {
		if err := c.selectMailbox(folder); err != nil {
			return err
		}
		sections := []*imap.FetchItemBodySection{
			{Part: part, Specifier: imap.PartSpecifierMIME, Peek: true},
			{Part: part, Peek: true},
		}
		var fetchSet imap.UIDSet
		fetchSet.AddNum(imap.UID(uid))
		msgs, err := conn.Fetch(fetchSet, &imap.FetchOptions{UID: true, BodySection: sections}).Collect()
		if err != nil {
			return fmt.Errorf("FETCH part %v uid=%d: %w", part, uid, err)
		}
		if len(msgs) == 0 {
			return fmt.Errorf("message uid=%d not found in %s", uid, folder)
		}
		var mime, body []byte
		for _, s := range msgs[0].BodySection {
			switch sectionKey(s.Section) {
			case sectionKey(sections[0]):
				mime = s.Bytes
			case sectionKey(sections[1]):
				body = s.Bytes
			}
		}
		if body == nil {
			return fmt.Errorf("FETCH part %v uid=%d: server returned no data", part, uid)
		}
		data = decodePart(mime, body)
		return nil
	})
	return data, err
}
