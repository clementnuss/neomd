package imap

// Lazy body fetch for large messages.
//
// Opening a 6 MB mail used to FETCH BODY.PEEK[] — the whole raw message,
// attachments included — and parse all of it before the reader could draw.
// The reader only needs the text parts; attachments are needed when the user
// presses 1–9, forwards, opens an .ics or renders inline images.
//
// For a multipart/mixed message at or above lazyBodyThreshold, FetchBodyOf
// fetches only the top-level header, the "body root" (the first non-attachment
// text/multipart child — the alternative/related tree with text, HTML and
// inline signature images) and any small sibling part, in ONE FETCH. The root
// is reassembled into a standalone message so the existing parseBody runs
// unchanged. Large siblings become Attachment entries with metadata only
// (Data == nil, Part set); FetchPart downloads one on demand.
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

// lazyPlan is what a multipart/mixed BODYSTRUCTURE lets us skip.
type lazyPlan struct {
	root   []int      // section path of the body root under the mixed container
	others []lazyPart // every other top-level child
}

type lazyPart struct {
	att   Attachment // metadata; Data filled only when eager
	eager bool       // small enough to fetch together with the body
}

// planLazy returns nil when bs is not a multipart/mixed container with an
// identifiable body root; callers then fall back to the full fetch.
func planLazy(bs imap.BodyStructure) *lazyPlan {
	mp, ok := bs.(*imap.BodyStructureMultiPart)
	if !ok || !strings.EqualFold(mp.Subtype, "mixed") {
		return nil
	}
	plan := &lazyPlan{}
	for i, child := range mp.Children {
		path := []int{i + 1}
		if plan.root == nil && isBodyRoot(child) {
			plan.root = path
			continue
		}
		plan.others = append(plan.others, lazyPartOf(path, child))
	}
	if plan.root == nil {
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

func lazyPartOf(path []int, bs imap.BodyStructure) lazyPart {
	switch p := bs.(type) {
	case *imap.BodyStructureSinglePart:
		ctype := strings.ToLower(p.MediaType())
		name := p.Filename()
		return lazyPart{
			att: Attachment{
				Filename:         name,
				ContentType:      ctype,
				ContentID:        strings.Trim(p.ID, "<>"),
				IsCalendarInvite: isCalendarPart(ctype, name),
				Size:             p.Size,
				Part:             path,
			},
			eager: p.Size <= lazyPartEagerMax,
		}
	case *imap.BodyStructureMultiPart:
		name := ""
		if d := p.Disposition(); d != nil {
			name = d.Params["filename"]
		}
		return lazyPart{att: Attachment{Filename: name, ContentType: strings.ToLower(p.MediaType()), Part: path}}
	}
	return lazyPart{att: Attachment{Part: path}}
}

// sectionKey identifies a fetched section regardless of pointer identity.
func sectionKey(s *imap.FetchItemBodySection) string {
	return fmt.Sprintf("%s/%v", s.Specifier, s.Part)
}

// reassembleRoot builds a standalone RFC 5322 message from the top-level
// header, the root part's MIME header and the root part's body: the top
// header keeps From/To/Subject/Date/List-*/References… while its Content-*
// fields are replaced by the root's, so parseBody sees exactly the text tree.
func reassembleRoot(top, rootMIME, body []byte) []byte {
	th := readHeaderLenient(top)
	rh := readHeaderLenient(rootMIME)
	for _, k := range []string{"Content-Type", "Content-Transfer-Encoding", "Content-Disposition", "Content-Id", "Content-Description", "Content-Location"} {
		th.Del(k)
	}
	fields := rh.Fields()
	for fields.Next() {
		th.Add(fields.Key(), fields.Value())
	}
	var buf bytes.Buffer
	_ = textproto.WriteHeader(&buf, th)
	buf.Write(body)
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
	err := c.withConnRetry(ctx, func(conn *imapclient.Client) error {
		attachments = nil // reset on retry
		if err := c.selectMailbox(folder); err != nil {
			return err
		}
		sections := []*imap.FetchItemBodySection{
			{Specifier: imap.PartSpecifierHeader, Peek: true},
			{Part: plan.root, Specifier: imap.PartSpecifierMIME, Peek: true},
			{Part: plan.root, Peek: true},
		}
		for _, p := range plan.others {
			if p.eager {
				sections = append(sections,
					&imap.FetchItemBodySection{Part: p.att.Part, Specifier: imap.PartSpecifierMIME, Peek: true},
					&imap.FetchItemBodySection{Part: p.att.Part, Peek: true})
			}
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
		top := got[sectionKey(sections[0])]
		rootMIME := got[sectionKey(sections[1])]
		rootBody, ok := got[sectionKey(sections[2])]
		if !ok || len(top) == 0 {
			return fmt.Errorf("FETCH body parts uid=%d: server returned no root section", uid)
		}
		markdown, rawHTML, webURL, attachments, references, spyPixels = parseBody(reassembleRoot(top, rootMIME, rootBody))
		for _, p := range plan.others {
			att := p.att
			if p.eager {
				mime := got[sectionKey(&imap.FetchItemBodySection{Part: att.Part, Specifier: imap.PartSpecifierMIME})]
				body := got[sectionKey(&imap.FetchItemBodySection{Part: att.Part})]
				att.Data = decodePart(mime, body)
			}
			attachments = append(attachments, att)
		}
		return nil
	})
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
