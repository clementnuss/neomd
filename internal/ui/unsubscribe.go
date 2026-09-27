package ui

import (
	"net/url"
	"strings"
)

// headerBlock returns the RFC 5322 header section of a raw message: everything
// before the first blank line, with CRLF normalised to LF and no trailing
// newline. Folded continuation lines are kept as-is so Received: chains stay
// readable in the <space>h view.
func headerBlock(raw []byte) string {
	s := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if i := strings.Index(s, "\n\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(s, "\n")
}

// headerValue returns the unfolded value of the first header named name in
// block (case-insensitive), or "" when absent.
func headerValue(block, name string) string {
	lines := strings.Split(block, "\n")
	for i, line := range lines {
		j := strings.IndexByte(line, ':')
		if j <= 0 || !strings.EqualFold(strings.TrimSpace(line[:j]), name) {
			continue
		}
		val := strings.TrimSpace(line[j+1:])
		for _, cont := range lines[i+1:] {
			if !strings.HasPrefix(cont, " ") && !strings.HasPrefix(cont, "\t") {
				break
			}
			val += " " + strings.TrimSpace(cont)
		}
		return val
	}
	return ""
}

// parseListUnsubscribe splits an RFC 2369 List-Unsubscribe value
// ("<url>, <url>") into the first https and the first mailto entry. Plain
// http entries are ignored: they are handed to the browser unencrypted and
// are rare on real lists.
func parseListUnsubscribe(v string) (https, mailto string) {
	for _, part := range strings.Split(v, ",") {
		u := strings.TrimSpace(part)
		u = strings.TrimSuffix(strings.TrimPrefix(u, "<"), ">")
		switch {
		case https == "" && strings.HasPrefix(u, "https://"):
			https = u
		case mailto == "" && strings.HasPrefix(u, "mailto:"):
			mailto = u
		}
	}
	return https, mailto
}

// parseMailto extracts the recipient and subject from a mailto: URL. The
// subject defaults to "unsubscribe" — what mailman-style list managers expect
// when the sender did not spell one out.
func parseMailto(s string) (to, subject string) {
	subject = "unsubscribe"
	u, err := url.Parse(s)
	if err != nil {
		return "", subject
	}
	to = u.Opaque
	if to == "" {
		to = u.Path
	}
	if sub := u.Query().Get("subject"); sub != "" {
		subject = sub
	}
	return to, subject
}

// findUnsubscribeLink returns the first body link whose text or URL mentions
// "unsubscribe" (case-insensitive) — the footer link most newsletters carry
// even when they omit the List-Unsubscribe header.
func findUnsubscribeLink(links []emailLink) string {
	for _, l := range links {
		if strings.Contains(strings.ToLower(l.Text), "unsubscribe") ||
			strings.Contains(strings.ToLower(l.URL), "unsubscribe") {
			return l.URL
		}
	}
	return ""
}

// weedOrder is the curated header set shown by the first <space>h press, in
// display order — the sender/routing/list facts a screener needs, without the
// Received chain, DKIM blobs and MIME plumbing (mutt's "weed" list).
var weedOrder = []string{
	"From", "Reply-To", "Sender", "To", "Cc", "Subject", "Date",
	"Message-ID", "In-Reply-To", "References",
	"Return-Path", "Precedence", "Auto-Submitted", "X-Mailer", "User-Agent",
	"Authentication-Results",
}

// weedPrefixes are matched case-insensitively against the header name and
// appended after weedOrder: every List-* header and every spam verdict.
var weedPrefixes = []string{"List-", "X-Spam"}

// weedHeaders filters a raw header block down to weedOrder (in that order)
// plus weedPrefixes matches (in original order). Folded continuation lines
// travel with their header. Headers absent from the block are skipped.
func weedHeaders(block string) string {
	lines := strings.Split(block, "\n")
	// Group each header with its continuation lines, remembering its name.
	type hdr struct{ name, text string }
	var hdrs []hdr
	for _, line := range lines {
		if (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) && len(hdrs) > 0 {
			hdrs[len(hdrs)-1].text += "\n" + line
			continue
		}
		name := line
		if j := strings.IndexByte(line, ':'); j > 0 {
			name = strings.TrimSpace(line[:j])
		}
		hdrs = append(hdrs, hdr{name: name, text: line})
	}
	var out []string
	for _, want := range weedOrder {
		for _, h := range hdrs {
			if strings.EqualFold(h.name, want) {
				out = append(out, h.text)
			}
		}
	}
	for _, h := range hdrs {
		for _, p := range weedPrefixes {
			if len(h.name) >= len(p) && strings.EqualFold(h.name[:len(p)], p) {
				out = append(out, h.text)
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
