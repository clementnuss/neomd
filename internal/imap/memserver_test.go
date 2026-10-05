package imap

// Protocol-level tests against go-imap's in-memory IMAP server. No network.
// The client under test is the real *Client (TLS to 127.0.0.1 with a
// self-signed cert; connect() retries loopback hosts insecurely via
// mailtls.ShouldRetryInsecureLocalhost, exactly as with Proton Bridge).
//
// TLS approach: Config{Host: "127.0.0.1", TLS: true} against a
// tls.NewListener-wrapped in-memory server works unmodified — connect()'s
// insecure-loopback retry (mailtls.ShouldRetryInsecureLocalhost) picks up
// the self-signed cert's x509.UnknownAuthorityError, so no TLSCertFile
// plumbing was needed.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"sort"
	"strconv"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// discardLogger swallows imapserver's error logging so test output stays
// pristine (imapserver.Options.Logger defaults to log.Default(), which
// writes to stderr, e.g. on connection-close during t.Cleanup).
type discardLogger struct{}

func (discardLogger) Printf(string, ...interface{}) {}

type memLiteral struct {
	*bytes.Reader
	n int64
}

func (l memLiteral) Size() int64 { return l.n }

func selfSignedLoopbackCert(t *testing.T) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "mem-imap"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// startMemIMAP starts an in-memory IMAP server with user "u"/"p" and the
// mailboxes INBOX, ToScreen, Archive, Trash, PaperTrail, Waiting, Scheduled,
// and returns a connected-on-first-use *Client plus the user for seeding.
func startMemIMAP(t *testing.T) (*Client, *imapmemserver.User) {
	t.Helper()
	mem := imapmemserver.New()
	user := imapmemserver.NewUser("u", "p")
	for _, mb := range []string{"INBOX", "ToScreen", "Archive", "Trash", "PaperTrail", "Waiting", "Scheduled"} {
		if err := user.Create(mb, nil); err != nil {
			t.Fatal(err)
		}
	}
	mem.AddUser(user)
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return mem.NewSession(), nil, nil
		},
		Caps:         goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapIMAP4rev2: {}},
		InsecureAuth: true,
		Logger:       discardLogger{},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{selfSignedLoopbackCert(t)}})
	go func() { _ = srv.Serve(tlsLn) }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	cli := New(Config{Host: "127.0.0.1", Port: port, User: "u", Password: "p", TLS: true})
	t.Cleanup(cli.Close)
	return cli, user
}

// seedMessage appends one message and returns nothing; UIDs are assigned in
// order, so the n-th seeded message in a mailbox has UID n.
func seedMessage(t *testing.T, user *imapmemserver.User, mailbox, subject string, seen bool) {
	t.Helper()
	raw := []byte("From: Sender <s@example.com>\r\nTo: u@example.com\r\nSubject: " + subject +
		"\r\nMessage-ID: <" + subject + "@example.com>\r\nDate: Mon, 01 Sep 2026 10:00:00 +0000\r\n\r\nbody\r\n")
	opts := &goimap.AppendOptions{Time: time.Now()}
	if seen {
		opts.Flags = []goimap.Flag{goimap.FlagSeen}
	}
	if _, err := user.Append(mailbox, memLiteral{bytes.NewReader(raw), int64(len(raw))}, opts); err != nil {
		t.Fatal(err)
	}
}

func uidsOf(emails []Email) []uint32 {
	out := make([]uint32, len(emails))
	for i, e := range emails {
		out[i] = e.UID
	}
	return out
}

func TestMem_FetchHeaders_NewestFirstLimited(t *testing.T) {
	cli, user := startMemIMAP(t)
	for i := 1; i <= 5; i++ {
		seedMessage(t, user, "INBOX", "m"+strconv.Itoa(i), i%2 == 0)
	}
	got, err := cli.FetchHeaders(context.Background(), "INBOX", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{5, 4, 3}
	if fmt.Sprint(uidsOf(got)) != fmt.Sprint(want) {
		t.Errorf("uids = %v, want %v", uidsOf(got), want)
	}
	if got[0].Subject != "m5" || got[0].From != "Sender <s@example.com>" {
		t.Errorf("envelope not decoded: %+v", got[0])
	}
	if !got[1].Seen || got[0].Seen {
		t.Errorf("flags wrong: uid4 seen=%v uid5 seen=%v", got[1].Seen, got[0].Seen)
	}
}

func TestMem_FetchHeaders_EmptyMailbox(t *testing.T) {
	cli, _ := startMemIMAP(t)
	got, err := cli.FetchHeaders(context.Background(), "Archive", 200)
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want empty, nil", got, err)
	}
}

func TestMem_FetchUnseenCounts(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMessage(t, user, "INBOX", "a", false)
	seedMessage(t, user, "INBOX", "b", false)
	seedMessage(t, user, "INBOX", "c", true)
	seedMessage(t, user, "Waiting", "d", false)
	counts, err := cli.FetchUnseenCounts(context.Background(), map[string]string{
		"Inbox": "INBOX", "PaperTrail": "PaperTrail", "Waiting": "Waiting", "Scheduled": "Scheduled", "Missing": "NoSuchBox",
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts["Inbox"] != 2 || counts["Waiting"] != 1 || counts["PaperTrail"] != 0 || counts["Scheduled"] != 0 {
		t.Errorf("counts = %v", counts)
	}
	if _, ok := counts["Missing"]; ok {
		t.Errorf("missing mailbox must be skipped, got %v", counts)
	}
}

func TestMem_MoveMessage_ThenFetchBothSides(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMessage(t, user, "INBOX", "keep", false)
	seedMessage(t, user, "INBOX", "go", false)
	destUID, err := cli.MoveMessage(context.Background(), "INBOX", 2, "Archive")
	if err != nil {
		t.Fatal(err)
	}
	if destUID != 1 {
		t.Errorf("destUID = %d, want 1 (COPYUID)", destUID)
	}
	src, _ := cli.FetchHeaders(context.Background(), "INBOX", 200)
	if fmt.Sprint(uidsOf(src)) != "[1]" {
		t.Errorf("source after move = %v, want [1]", uidsOf(src))
	}
	dst, _ := cli.FetchHeaders(context.Background(), "Archive", 200)
	if len(dst) != 1 || dst[0].Subject != "go" {
		t.Errorf("dest after move = %+v", dst)
	}
}

func TestMem_SearchUIDs_And_FetchHeadersByUID(t *testing.T) {
	cli, user := startMemIMAP(t)
	for i := 1; i <= 4; i++ {
		seedMessage(t, user, "ToScreen", "t"+strconv.Itoa(i), false)
	}
	uids, err := cli.SearchUIDs(context.Background(), "ToScreen")
	if err != nil || fmt.Sprint(uids) != "[1 2 3 4]" {
		t.Fatalf("SearchUIDs = %v, %v", uids, err)
	}
	got, err := cli.FetchHeadersByUID(context.Background(), "ToScreen", []uint32{2, 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Subject != "t2" || got[1].Subject != "t4" {
		t.Errorf("FetchHeadersByUID = %+v", got)
	}
}

func TestMem_FetchHeaders_MissingMailboxKeepsConnectionUsable(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMessage(t, user, "INBOX", "x", false)
	if _, err := cli.FetchHeaders(context.Background(), "NoSuchBox", 10); err == nil {
		t.Fatal("expected SELECT error for missing mailbox")
	}
	// The pipelined UID SEARCH response must have been drained: the very next
	// call on the same connection works and selects the right mailbox.
	got, err := cli.FetchHeaders(context.Background(), "INBOX", 10)
	if err != nil || len(got) != 1 || got[0].Subject != "x" {
		t.Errorf("after failed SELECT: got %+v, %v", got, err)
	}
	if cli.selectedMailbox != "INBOX" {
		t.Errorf("selectedMailbox = %q, want INBOX", cli.selectedMailbox)
	}
}

func TestMem_FetchHeaders_SecondCallSkipsSelect(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMessage(t, user, "INBOX", "x", false)
	if _, err := cli.FetchHeaders(context.Background(), "INBOX", 10); err != nil {
		t.Fatal(err)
	}
	// Selection is cached; a second call must not error and must still see new mail.
	seedMessage(t, user, "INBOX", "y", false)
	got, err := cli.FetchHeaders(context.Background(), "INBOX", 10)
	if err != nil || fmt.Sprint(uidsOf(got)) != "[2 1]" {
		t.Errorf("second call: %v, %v", uidsOf(got), err)
	}
}

func TestMem_MoveMessage_KeepsSelectionAndBatchWorks(t *testing.T) {
	cli, user := startMemIMAP(t)
	for i := 1; i <= 4; i++ {
		seedMessage(t, user, "INBOX", "m"+strconv.Itoa(i), false)
	}
	if _, err := cli.FetchHeaders(context.Background(), "INBOX", 10); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []uint32{2, 3, 4} {
		if _, err := cli.MoveMessage(context.Background(), "INBOX", uid, "Trash"); err != nil {
			t.Fatalf("move %d: %v", uid, err)
		}
		if cli.selectedMailbox != "INBOX" {
			t.Fatalf("after MOVE selectedMailbox = %q, want INBOX (no forced re-SELECT)", cli.selectedMailbox)
		}
	}
	src, _ := cli.FetchHeaders(context.Background(), "INBOX", 10)
	if fmt.Sprint(uidsOf(src)) != "[1]" {
		t.Errorf("INBOX after 3 moves = %v, want [1]", uidsOf(src))
	}
	dst, _ := cli.FetchHeaders(context.Background(), "Trash", 10)
	if fmt.Sprint(uidsOf(dst)) != "[3 2 1]" {
		t.Errorf("Trash after 3 moves = %v, want [3 2 1]", uidsOf(dst))
	}
}

func TestMem_FailedSelectClearsCachedSelection(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMessage(t, user, "INBOX", "x", false)
	if _, err := cli.FetchHeaders(context.Background(), "INBOX", 10); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.MoveMessage(context.Background(), "NoSuchBox", 1, "Trash"); err == nil {
		t.Fatal("expected SELECT error for missing mailbox")
	}
	if cli.selectedMailbox != "" {
		t.Errorf("selectedMailbox = %q after failed SELECT, want empty", cli.selectedMailbox)
	}
}

// seedMessageAt appends one message with an explicit INTERNALDATE. The Date
// header matches so envelope-date sorting agrees with the internal date.
func seedMessageAt(t *testing.T, user *imapmemserver.User, mailbox, subject string, at time.Time) {
	t.Helper()
	raw := []byte("From: Sender <s@example.com>\r\nTo: u@example.com\r\nSubject: " + subject +
		"\r\nMessage-ID: <" + subject + "@example.com>\r\nDate: " + at.UTC().Format(time.RFC1123Z) + "\r\n\r\nbody\r\n")
	opts := &goimap.AppendOptions{Time: at}
	if _, err := user.Append(mailbox, memLiteral{bytes.NewReader(raw), int64(len(raw))}, opts); err != nil {
		t.Fatal(err)
	}
}

// seedMovedInOldMail reproduces issue #34: three recent mails arrive in INBOX
// (UIDs 1–3), then three old mails queued in ToScreen are MOVEd back into
// INBOX by a bulk approve and receive fresh, higher UIDs (4–6) although
// their INTERNALDATE is months older. The highest UIDs are the oldest mail.
func seedMovedInOldMail(t *testing.T, cli *Client, user *imapmemserver.User) {
	t.Helper()
	recent := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		seedMessageAt(t, user, "INBOX", "new"+strconv.Itoa(i), recent.Add(time.Duration(i)*time.Hour))
	}
	old := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= 3; i++ {
		seedMessageAt(t, user, "ToScreen", "old"+strconv.Itoa(i), old.Add(time.Duration(i)*time.Hour))
	}
	for uid := uint32(1); uid <= 3; uid++ {
		destUID, err := cli.MoveMessage(context.Background(), "ToScreen", uid, "INBOX")
		if err != nil {
			t.Fatal(err)
		}
		if destUID != uid+3 {
			t.Fatalf("moved old%d landed at INBOX uid %d, want %d", uid, destUID, uid+3)
		}
	}
}

func TestMem_FetchHeaders_WindowIsNewestByInternalDateNotUID(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMovedInOldMail(t, cli, user)
	got, err := cli.FetchHeaders(context.Background(), "INBOX", 3)
	if err != nil {
		t.Fatal(err)
	}
	// Newest first by arrival date: new3, new2, new1 — not the moved-in old mail.
	want := []uint32{3, 2, 1}
	if fmt.Sprint(uidsOf(got)) != fmt.Sprint(want) {
		t.Errorf("uids = %v, want %v (window must be selected by INTERNALDATE, not UID)", uidsOf(got), want)
	}
}

func TestMem_FetchHeaders_FolderWithinLimitUnchanged(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMovedInOldMail(t, cli, user)
	got, err := cli.FetchHeaders(context.Background(), "INBOX", 200)
	if err != nil {
		t.Fatal(err)
	}
	// No window needed: every message, UID-descending as before (the UI sorts).
	want := []uint32{6, 5, 4, 3, 2, 1}
	if fmt.Sprint(uidsOf(got)) != fmt.Sprint(want) {
		t.Errorf("uids = %v, want %v", uidsOf(got), want)
	}
}

func TestMem_FetchLatest_WindowIsNewestByInternalDateNotUID(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMovedInOldMail(t, cli, user)
	got, err := cli.FetchLatest(context.Background(), "INBOX", 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{3, 2, 1}
	if fmt.Sprint(uidsOf(got)) != fmt.Sprint(want) {
		t.Errorf("uids = %v, want %v", uidsOf(got), want)
	}
}

func TestMem_SearchFolder_CapIsNewestByInternalDateNotUID(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMovedInOldMail(t, cli, user)
	old := searchFolderCap
	searchFolderCap = 3
	t.Cleanup(func() { searchFolderCap = old })
	got, err := cli.searchFolder(context.Background(), "INBOX", &goimap.SearchCriteria{})
	if err != nil {
		t.Fatal(err)
	}
	// FetchHeadersByUID returns server order; the search view sorts by date.
	uids := uidsOf(got)
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	want := []uint32{1, 2, 3}
	if fmt.Sprint(uids) != fmt.Sprint(want) {
		t.Errorf("uids = %v, want %v (cap must keep the most recently received, not highest UIDs)", uids, want)
	}
}

func TestMem_FetchMoreHeaders_SkipsLoadedAndPicksNewestRemaining(t *testing.T) {
	cli, user := startMemIMAP(t)
	seedMovedInOldMail(t, cli, user)
	// The list already holds the first window (new1–new3); the next page is
	// the two most recently received of the rest: old3 (uid 6), old2 (uid 5).
	loaded := map[uint32]bool{1: true, 2: true, 3: true}
	got, err := cli.FetchMoreHeaders(context.Background(), "INBOX", 2, loaded)
	if err != nil {
		t.Fatal(err)
	}
	want := []uint32{6, 5}
	if fmt.Sprint(uidsOf(got)) != fmt.Sprint(want) {
		t.Errorf("uids = %v, want %v", uidsOf(got), want)
	}
	// Everything loaded → nothing more, no error.
	for _, e := range got {
		loaded[e.UID] = true
	}
	loaded[4] = true
	rest, err := cli.FetchMoreHeaders(context.Background(), "INBOX", 2, loaded)
	if err != nil || len(rest) != 0 {
		t.Errorf("after all loaded: %v, %v; want empty", uidsOf(rest), err)
	}
}
