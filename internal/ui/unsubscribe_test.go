package ui

import "testing"

func TestHeaderBlockStopsAtFirstBlankLine(t *testing.T) {
	raw := []byte("From: a@b.c\r\nSubject: hi\r\n Folded: yes\r\n\r\nbody text\r\n\r\nmore body\r\n")
	got := headerBlock(raw)
	want := "From: a@b.c\nSubject: hi\n Folded: yes"
	if got != want {
		t.Errorf("headerBlock = %q, want %q", got, want)
	}
}

func TestHeaderBlockNoBodyReturnsAll(t *testing.T) {
	raw := []byte("From: a@b.c\nSubject: hi")
	if got := headerBlock(raw); got != "From: a@b.c\nSubject: hi" {
		t.Errorf("headerBlock = %q", got)
	}
}

func TestHeaderValueFindsCaseInsensitiveAndUnfolds(t *testing.T) {
	block := "From: a@b.c\nlist-unsubscribe: <https://x.example/u?id=1>,\n <mailto:u@x.example>\nSubject: hi"
	got := headerValue(block, "List-Unsubscribe")
	want := "<https://x.example/u?id=1>, <mailto:u@x.example>"
	if got != want {
		t.Errorf("headerValue = %q, want %q", got, want)
	}
	if got := headerValue(block, "X-Missing"); got != "" {
		t.Errorf("missing header = %q, want empty", got)
	}
}

func TestParseListUnsubscribeSplitsHTTPSAndMailto(t *testing.T) {
	https, mailto := parseListUnsubscribe("<mailto:list-request@x.example?subject=unsubscribe>, <https://x.example/u?id=1>")
	if https != "https://x.example/u?id=1" {
		t.Errorf("https = %q", https)
	}
	if mailto != "mailto:list-request@x.example?subject=unsubscribe" {
		t.Errorf("mailto = %q", mailto)
	}
}

func TestParseListUnsubscribeIgnoresPlainHTTP(t *testing.T) {
	// Only https is offered to the browser; plain http links are not trusted.
	https, mailto := parseListUnsubscribe("<http://x.example/u>")
	if https != "" || mailto != "" {
		t.Errorf("got %q %q, want empty", https, mailto)
	}
}

func TestParseMailtoSplitsAddressAndSubject(t *testing.T) {
	to, subject := parseMailto("mailto:list-request@x.example?subject=unsubscribe%20me&body=x")
	if to != "list-request@x.example" {
		t.Errorf("to = %q", to)
	}
	if subject != "unsubscribe me" {
		t.Errorf("subject = %q", subject)
	}
	to, subject = parseMailto("mailto:list-request@x.example")
	if to != "list-request@x.example" || subject != "unsubscribe" {
		t.Errorf("default: to=%q subject=%q", to, subject)
	}
}

func TestFindUnsubscribeLinkMatchesTextOrURL(t *testing.T) {
	links := []emailLink{
		{Text: "View online", URL: "https://x.example/view"},
		{Text: "Manage preferences", URL: "https://x.example/UNSUBSCRIBE?u=1"},
		{Text: "Unsubscribe", URL: "https://x.example/bye"},
	}
	if got := findUnsubscribeLink(links); got != "https://x.example/UNSUBSCRIBE?u=1" {
		t.Errorf("findUnsubscribeLink = %q", got)
	}
	if got := findUnsubscribeLink(links[:1]); got != "" {
		t.Errorf("no match = %q, want empty", got)
	}
}

func TestWeedHeadersKeepsKeyHeadersInFixedOrderWithFolding(t *testing.T) {
	block := "Return-Path: <b@x.example>\n" +
		"Received: from a\n by b\n" +
		"DKIM-Signature: v=1;\n b=abc\n" +
		"Date: Fri, 4 Sep 2026 18:04:32 +0000\n" +
		"From: Omarchy Weekly <pombo@x.example>\n" +
		"To: simu@sspaeti.com\n" +
		"Subject: hi\n" +
		"Content-Type: multipart/alternative;\n boundary=\"x\"\n" +
		"List-ID: Omarchy Weekly <pombo@x.example>\n" +
		"list-unsubscribe: <https://x.example/u>,\n <mailto:u@x.example>\n" +
		"X-Hostpoint-Spambox:  YES\n" +
		"X-Spam-Status: No\n"
	got := weedHeaders(block)
	want := "From: Omarchy Weekly <pombo@x.example>\n" +
		"To: simu@sspaeti.com\n" +
		"Subject: hi\n" +
		"Date: Fri, 4 Sep 2026 18:04:32 +0000\n" +
		"Return-Path: <b@x.example>\n" +
		"List-ID: Omarchy Weekly <pombo@x.example>\n" +
		"list-unsubscribe: <https://x.example/u>,\n <mailto:u@x.example>\n" +
		"X-Spam-Status: No"
	if got != want {
		t.Errorf("weedHeaders =\n%s\nwant\n%s", got, want)
	}
}
