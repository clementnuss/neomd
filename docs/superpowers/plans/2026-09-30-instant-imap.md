# Instant IMAP Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make folder switches, screening, deletes and moves feel instant on any network by cutting sequential IMAP round trips and moving the list redraw off the network path, without changing any server-side operation.

**Architecture:** Four independent layers, each its own commit: (0) an opt-in per-operation timing trace; (1) pipelined SELECT/SEARCH/STATUS and no forced re-SELECT after MOVE in `internal/imap/client.go`; (2) a second `*imap.Client` per account for background housekeeping; (3) optimistic list removal in `internal/ui/model.go` with a full server reload on any error; (4) a per-folder header cache shown with a visible `↻` while the real fetch runs, plus a background prefetch. Every layer is pinned by protocol tests against go-imap's in-memory server, bubbletea `Update`-driven workflow tests, and live integration tests.

**Tech Stack:** Go 1.22+, bubbletea v1.3.10, go-imap/v2 v2.0.0-beta.8 (`imapclient`, `imapserver/imapmemserver`), BurntSushi/toml.

**Spec:** `docs/superpowers/specs/2026-09-30-instant-imap-design.md`

## Global Constraints

- Keep diffs minimal; do not refactor adjacent code (CLAUDE.md).
- No new keybindings. No modifier keys.
- Never bare `go func()` in `internal/ui` — use `safeGo()`.
- Mutating IMAP ops (MOVE/APPEND/STORE) go through `withConn` (no retry); read-only ops through `withConnRetry` (AGENTS.md).
- Every server-side MOVE/EXPUNGE still writes its `audit(...)` line; the audit lines must be byte-identical to today's.
- Undo semantics unchanged: `destUID = 0` when the server sends no COPYUID; `undoableMoves` skips those.
- `imap_disabled = true` accounts have nil clients; every new client helper must handle nil (`internal/ui/imap_client_helpers_test.go` style).
- Any error in an optimistic path ends in `m.loading = true` + `fetchFolderCmd(m.activeFolder())` (full reload) with the error in `m.status`.
- A cached list is never displayed without `m.refreshing = true` and a fetch in flight.
- Every user-visible change: `AGENTS.md` invariant + `CHANGELOG.md` entry with regression test names.
- After any task touching `internal/imap` or the move path: `go test ./... -run Hardening` and `make test-integration` (AGENTS.md Hardening Suite rule; the integration run needs `IMAP_PASS_NEOMD_DEMO` in the environment).
- Work on branch `speed-improvements` off `dev`. Commit after every task. `go test ./...` and `go vet ./...` green before each commit. `gofmt -w` on touched files.
- Commit messages end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.

## Review Focus

1. **SELECT fails while UID SEARCH is already pipelined behind it** (folder renamed/deleted on the server): the SEARCH must be drained and the connection reused for the next call without a reconnect. Pinned in Task 4 (`TestMem_FetchHeaders_MissingMailboxKeepsConnectionUsable`).
2. **A MOVE fails halfway through a batch after rows were already removed from the list**: the user must see the error and a server-truth list, and the already-moved rows must be undoable. Pinned in Task 7 (`TestOptimistic_BatchErrorReloadsAndKeepsPartialUndo`).
3. **A late `emailsLoadedMsg` for a folder the user has already left**: it must never overwrite the visible list. Pinned in Task 8 (`TestCache_LateResultForOtherFolderIsCachedNotShown`).
4. **Account switch with the same folder name in both accounts** (both have `INBOX`): the cache must never show account A's mail under account B. Pinned in Task 8 (`TestCache_KeyedByAccount`).
5. **Background sync mid-cycle while the user loads the Inbox**: the load must not issue a second MOVE for mail the sync is already moving. Pinned in Task 6 (`TestInboxLoadSkipsAutoScreenWhileBgSyncRuns`).

---

## File structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` | `IMAPTracePath()`; `UIConfig.InstantFolderSwitch *bool` + `InstantSwitch() bool`. |
| `internal/config/config_test.go` | Tests for the two additions. |
| `internal/imap/trace.go` (new) | `SetTracePath`, `trace(op string, start time.Time)`; opt-in timing log. |
| `internal/imap/trace_test.go` (new) | Trace on/off tests. |
| `internal/imap/client.go` | `beginSelect`/`endSelect`; pipelined `FetchUnseenCounts`, `FetchHeaders`, `SearchUIDs`, `FetchHeadersByUID`, `searchFolder`; `MoveMessage` keeps selection; `defer trace(...)` lines. |
| `internal/imap/memserver_test.go` (new) | In-memory IMAP server harness + protocol tests. |
| `internal/integration_test.go` | Live tests: pipelined fetch equality, move visibility without re-SELECT, STATUS counts. |
| `cmd/neomd/main.go` | Build `bgClients`, pass via `WithBackgroundClients`, close on exit, enable trace when `NEOMD_IMAP_TRACE=1`. |
| `internal/ui/model.go` | `bgClients`, `bgImapCli()`, `WithBackgroundClients`; background cmds use `bgImapCli()`; `removeFromList`, `refreshActiveFolderCmd`, `refreshing`; `folderCache`, `loadActiveFolder`, `emailsLoadedMsg` guard, prefetch chain; `↻` in `viewInbox`; bulk progress in status. |
| `internal/ui/instant_test.go` (new) | All Layer 2–4 workflow tests. |
| `internal/ui/imap_client_helpers_test.go` | `bgImapCli` nil-safety tests. |
| `internal/ui/model_test.go` | `TestReloadKeepsCursorOnSameEmail` uses the active folder name. |
| `AGENTS.md`, `CHANGELOG.md`, `docs/content/docs/configuration/_index.md`, `README.md` | Docs. |

---

### Task 0: Baseline

**Files:** none changed.

- [ ] **Step 1: Branch**

```bash
git checkout speed-improvements
```

- [ ] **Step 2: Full green baseline**

Run: `go test ./... && go vet ./... && go test ./... -run Hardening`
Expected: all PASS.

- [ ] **Step 3: Live baseline**

Run: `make test-integration` (needs `IMAP_PASS_NEOMD_DEMO`).
Expected: all `TestIntegration_*` PASS. If the environment variable is missing, note it in the task report and continue; Task 5 and Task 10 require it.

- [ ] **Step 4: Record today's numbers**

Paste this table (measured 2026-09-30 on a saturated Wi-Fi link, NOOP 250–450 ms) into `docs/superpowers/plans/2026-09-30-instant-imap.md` under a new heading `## Baseline numbers` at the end of the file, to be extended by Task 10:

```
| Op (Hostpoint demo)            | Serial today | Pipelined |
|--------------------------------|-------------:|----------:|
| 4× STATUS                      | 1114 ms      | 204 ms    |
| Switch Inbox (SELECT,SEARCH,FETCH) | 883 ms   | 517 ms    |
| Switch ToScreen                | 1238 ms      | 655 ms    |
```

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/plans/2026-09-30-instant-imap.md
git commit -m "plan: instant imap baseline numbers

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 1: Layer 0 — opt-in IMAP timing trace

**Files:**
- Create: `internal/imap/trace.go`, `internal/imap/trace_test.go`
- Modify: `internal/config/config.go` (after `AuditLogPath`, line ~482), `internal/config/config_test.go`, `internal/imap/client.go` (top of `FetchHeaders`, `FetchHeadersByUID`, `SearchUIDs`, `FetchUnseenCounts`, `FetchBody`, `MoveMessage`, `ExpungeAll`, `searchFolder`, `MarkSeen`, `MarkUnseen`), `cmd/neomd/main.go:55`

**Interfaces:**
- Produces: `imap.SetTracePath(p string)`, package-private `trace(op string, start time.Time)`, `config.IMAPTracePath() string`.

- [ ] **Step 1: Write the failing tests**

`internal/imap/trace_test.go`:

```go
package imap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTrace_WritesLineWhenEnabled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "imap-trace.log")
	SetTracePath(p)
	defer SetTracePath("")
	trace("FetchHeaders INBOX n=200", time.Now().Add(-3*time.Millisecond))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("trace file not written: %v", err)
	}
	line := strings.TrimSpace(string(b))
	if !strings.Contains(line, " FetchHeaders INBOX n=200 ") || !strings.HasSuffix(line, "ms") {
		t.Errorf("unexpected trace line %q", line)
	}
	if _, err := time.Parse(time.RFC3339Nano, strings.Fields(line)[0]); err != nil {
		t.Errorf("first field is not RFC3339Nano: %q", line)
	}
}

func TestTrace_NoopWhenDisabled(t *testing.T) {
	p := filepath.Join(t.TempDir(), "imap-trace.log")
	SetTracePath("")
	trace("Ping", time.Now())
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("trace file must not exist when disabled")
	}
}
```

`internal/config/config_test.go` (append):

```go
func TestIMAPTracePath_NextToMovesLog(t *testing.T) {
	if filepath.Dir(IMAPTracePath()) != filepath.Dir(AuditLogPath()) {
		t.Errorf("IMAPTracePath() = %q, want same dir as AuditLogPath() %q", IMAPTracePath(), AuditLogPath())
	}
	if filepath.Base(IMAPTracePath()) != "imap-trace.log" {
		t.Errorf("basename = %q, want imap-trace.log", filepath.Base(IMAPTracePath()))
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/imap -run 'TestTrace_' ; go test ./internal/config -run TestIMAPTracePath`
Expected: FAIL, `undefined: SetTracePath`, `undefined: IMAPTracePath`.

- [ ] **Step 3: Implement**

`internal/imap/trace.go`:

```go
package imap

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// tracePath is the file every public IMAP operation appends a timing line
// to when NEOMD_IMAP_TRACE=1 (set via SetTracePath, normally
// ~/.cache/neomd/imap-trace.log). Empty = disabled, zero cost.
var (
	traceMu   sync.Mutex
	tracePath string
)

// SetTracePath enables (non-empty) or disables ("") the IMAP timing trace.
func SetTracePath(p string) {
	traceMu.Lock()
	defer traceMu.Unlock()
	tracePath = p
}

// trace appends "<RFC3339Nano> <op> <elapsed>ms". Use as
// `defer trace("FetchHeaders "+folder, time.Now())`. Failures are ignored —
// the trace must never block or fail a mail operation.
func trace(op string, start time.Time) {
	traceMu.Lock()
	p := tracePath
	traceMu.Unlock()
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s %dms\n", time.Now().Format(time.RFC3339Nano), op, time.Since(start).Milliseconds())
}
```

`internal/config/config.go`, after `AuditLogPath`:

```go
// IMAPTracePath returns ~/.cache/neomd/imap-trace.log, the opt-in
// (NEOMD_IMAP_TRACE=1) per-operation timing log. Same directory as moves.log.
func IMAPTracePath() string {
	if dir, err := os.UserCacheDir(); err == nil {
		p := filepath.Join(dir, cacheDirName)
		_ = os.MkdirAll(p, 0o700)
		return filepath.Join(p, "imap-trace.log")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("neomd_%d_imap-trace.log", os.Getuid()))
}
```

`cmd/neomd/main.go`, right after `goIMAP.SetAuditLogPath(...)`:

```go
	if os.Getenv("NEOMD_IMAP_TRACE") == "1" {
		goIMAP.SetTracePath(config.IMAPTracePath()) // per-operation timings → ~/.cache/neomd/imap-trace.log
	}
```

`internal/imap/client.go` — add as the first statement of each listed method (after the `ctx == nil` guard is fine too):

```go
	defer trace(fmt.Sprintf("FetchHeaders %s n=%d", folder, n), time.Now())            // FetchHeaders
	defer trace(fmt.Sprintf("FetchHeadersByUID %s count=%d", folder, len(uids)), time.Now()) // FetchHeadersByUID
	defer trace("SearchUIDs "+folder, time.Now())                                         // SearchUIDs
	defer trace(fmt.Sprintf("FetchUnseenCounts %d folders", len(folders)), time.Now())    // FetchUnseenCounts
	defer trace(fmt.Sprintf("FetchBody %s uid=%d", folder, uid), time.Now())              // FetchBody
	defer trace(fmt.Sprintf("MoveMessage %s uid=%d -> %s", src, uid, dst), time.Now())    // MoveMessage
	defer trace(fmt.Sprintf("ExpungeAll %s count=%d", folder, len(uids)), time.Now())     // ExpungeAll
	defer trace("searchFolder "+folder, time.Now())                                       // searchFolder
	defer trace(fmt.Sprintf("MarkSeen %s uid=%d", folder, uid), time.Now())               // MarkSeen
	defer trace(fmt.Sprintf("MarkUnseen %s uid=%d", folder, uid), time.Now())             // MarkUnseen
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/imap ./internal/config && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Smoke the trace on the demo account**

Run: `make build && NEOMD_IMAP_TRACE=1 ./neomd -config ~/.config/neomd-demo-hostpoint/config.toml` — press `Tab` twice, `q`. Then `tail -5 ~/.cache/neomd/imap-trace.log`.
Expected: lines like `… FetchHeaders ToScreen n=200 187ms` and `… FetchUnseenCounts 4 folders 41ms`.

- [ ] **Step 6: Commit**

```bash
git add internal/imap/trace.go internal/imap/trace_test.go internal/imap/client.go internal/config/config.go internal/config/config_test.go cmd/neomd/main.go
git commit -m "imap: opt-in per-operation timing trace (NEOMD_IMAP_TRACE=1)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: In-memory IMAP server harness + pins of today's behavior

**Files:**
- Create: `internal/imap/memserver_test.go`

**Interfaces:**
- Produces (test-only): `startMemIMAP(t) (*Client, *imapmemserver.User)`, `seedMessage(t, user, mailbox, subject string, seen bool) `, `uidsOf([]Email) []uint32`.

- [ ] **Step 1: Write the harness and the first tests**

```go
package imap

// Protocol-level tests against go-imap's in-memory IMAP server. No network.
// The client under test is the real *Client (TLS to 127.0.0.1 with a
// self-signed cert; connect() retries loopback hosts insecurely via
// mailtls.ShouldRetryInsecureLocalhost, exactly as with Proton Bridge).

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
	"strconv"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

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
		Caps: goimap.CapSet{goimap.CapIMAP4rev1: {}, goimap.CapIMAP4rev2: {}},
		InsecureAuth: true,
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
```

- [ ] **Step 2: Run; all must pass on today's code**

Run: `go test ./internal/imap -run 'TestMem_' -v`
Expected: PASS for all five. If the TLS handshake fails with a non-x509 error, replace `TLS: true` with writing the cert PEM to `t.TempDir()` and passing `TLSCertFile`; either is acceptable, keep whichever passes and note it in the harness comment.

- [ ] **Step 3: Commit**

```bash
git add internal/imap/memserver_test.go
git commit -m "imap: in-memory IMAP server harness pinning FetchHeaders/STATUS/MOVE behavior

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Layer 1a — pipelined STATUS

**Files:**
- Modify: `internal/imap/client.go:529-556` (`FetchUnseenCounts`)

- [ ] **Step 1: Confirm pin exists**

`TestMem_FetchUnseenCounts` (Task 2) is the behavior pin. No new test is needed for correctness; the round-trip saving is measured in Task 10.

- [ ] **Step 2: Implement**

Replace the loop body of `FetchUnseenCounts`:

```go
	err := c.withConnRetry(ctx, func(conn *imapclient.Client) error {
		counts = make(map[string]int, len(folders)) // reset on retry
		// Send every STATUS before waiting for any: one round trip instead of
		// one per folder. STATUS depends on nothing the client has to read first.
		type inflight struct {
			label string
			cmd   *imapclient.StatusCommand
		}
		cmds := make([]inflight, 0, len(folders))
		for label, mailbox := range folders {
			cmds = append(cmds, inflight{label, conn.Status(mailbox, &imap.StatusOptions{NumUnseen: true})})
		}
		var netErr error
		for _, f := range cmds {
			data, err := f.cmd.Wait()
			if err != nil {
				if isNetErr(err) && netErr == nil {
					netErr = err // keep draining the rest, then let withConnRetry reconnect
				}
				continue // folder may not exist; skip
			}
			if data.NumUnseen != nil {
				counts[f.label] = int(*data.NumUnseen)
			}
		}
		return netErr
	})
```

- [ ] **Step 3: Run tests**

Run: `go test ./internal/imap && go vet ./internal/imap`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/imap/client.go
git commit -m "imap: pipeline STATUS for tab counts (4 round trips -> 1)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Layer 1b — SELECT pipelined with the first command

**Files:**
- Modify: `internal/imap/client.go` (`selectMailbox` area line ~292; `FetchHeaders` 371-380; `SearchUIDs` 498-510; `FetchHeadersByUID` 872-878; `searchFolder` 592-604)
- Test: `internal/imap/memserver_test.go`

**Interfaces:**
- Produces: `(c *Client) beginSelect(conn *imapclient.Client, folder string) *imapclient.SelectCommand`, `(c *Client) endSelect(cmd *imapclient.SelectCommand, folder string) error`.

- [ ] **Step 1: Write the failing test** (append to `memserver_test.go`)

```go
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
```

- [ ] **Step 2: Run to verify the first fails**

Run: `go test ./internal/imap -run 'TestMem_FetchHeaders_' -v`
Expected: `TestMem_FetchHeaders_MissingMailboxKeepsConnectionUsable` may already pass on serial code (the error path is the same); `TestMem_FetchHeaders_SecondCallSkipsSelect` passes. That is fine: both become regression pins for the pipelined version. Proceed.

- [ ] **Step 3: Implement the helpers** (next to `selectMailbox`)

```go
// beginSelect sends SELECT without waiting when folder is not the cached
// selection, so the caller can pipeline its first command behind it (RFC
// 9051 §5.5). Returns nil when no SELECT was needed.
func (c *Client) beginSelect(conn *imapclient.Client, folder string) *imapclient.SelectCommand {
	if c.selectedMailbox == folder {
		return nil
	}
	return conn.Select(folder, nil)
}

// endSelect waits for a beginSelect command and records the selection only
// on success. The caller must still Wait() its pipelined command afterwards
// (to drain its response) even when this returns an error.
func (c *Client) endSelect(cmd *imapclient.SelectCommand, folder string) error {
	if cmd == nil {
		return nil
	}
	if _, err := cmd.Wait(); err != nil {
		c.selectedMailbox = ""
		return fmt.Errorf("SELECT %q: %w", folder, err)
	}
	c.selectedMailbox = folder
	return nil
}
```

`FetchHeaders` — replace the `selectMailbox` + `UIDSearch` block:

```go
		sel := c.beginSelect(conn, folder)
		srch := conn.UIDSearch(&imap.SearchCriteria{}, nil)
		if err := c.endSelect(sel, folder); err != nil {
			_, _ = srch.Wait() // drain the pipelined response
			return err
		}
		searchData, err := srch.Wait()
		if err != nil {
			return fmt.Errorf("UID SEARCH: %w", err)
		}
```

`SearchUIDs` — same replacement (identical four lines, `criteria` is `&imap.SearchCriteria{}`).

`searchFolder` — same replacement with `conn.UIDSearch(criteria, nil)`.

`FetchHeadersByUID` — replace `selectMailbox` + `Fetch(...)`:

```go
		sel := c.beginSelect(conn, folder)
		var fetchSet imap.UIDSet
		for _, uid := range uids {
			fetchSet.AddNum(imap.UID(uid))
		}
		fetchCmd := conn.Fetch(fetchSet, &imap.FetchOptions{
			UID:           true,
			Flags:         true,
			Envelope:      true,
			RFC822Size:    true,
			BodyStructure: &imap.FetchItemBodyStructure{Extended: true},
			BodySection:   sendAtHeaderSection(),
		})
		if err := c.endSelect(sel, folder); err != nil {
			_, _ = fetchCmd.Collect()
			return err
		}
		msgs, err := fetchCmd.Collect()
		if err != nil {
			return fmt.Errorf("FETCH headers: %w", err)
		}
```

- [ ] **Step 4: Run all imap tests**

Run: `go test ./internal/imap -v -run 'TestMem_' && go test ./internal/imap && go vet ./internal/imap`
Expected: PASS.

- [ ] **Step 5: Live equality test** (append to `internal/integration_test.go`)

```go
// TestIntegration_PipelinedFetchMatchesSearch pins that the pipelined
// SELECT‖UID SEARCH path in FetchHeaders returns exactly the newest-n UID set
// a serial SearchUIDs + FetchHeadersByUID sees on a real server.
func TestIntegration_PipelinedFetchMatchesSearch(t *testing.T) {
	env := loadEnv(t)
	cli := env.imapClient()
	defer cli.Close()
	ctx := context.Background()
	fast, err := cli.FetchHeaders(ctx, "INBOX", 50)
	if err != nil {
		t.Fatal(err)
	}
	uids, err := cli.SearchUIDs(ctx, "INBOX")
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) > 50 {
		uids = uids[len(uids)-50:]
	}
	slow, err := cli.FetchHeadersByUID(ctx, "INBOX", uids)
	if err != nil {
		t.Fatal(err)
	}
	if len(fast) != len(slow) {
		t.Fatalf("pipelined %d emails, serial %d", len(fast), len(slow))
	}
	for i := range fast { // fast is newest-first, slow ascending
		if fast[i].UID != slow[len(slow)-1-i].UID || fast[i].Subject != slow[len(slow)-1-i].Subject {
			t.Errorf("row %d differs: %d/%q vs %d/%q", i, fast[i].UID, fast[i].Subject, slow[len(slow)-1-i].UID, slow[len(slow)-1-i].Subject)
		}
	}
	counts, err := cli.FetchUnseenCounts(ctx, map[string]string{"Inbox": "INBOX", "PaperTrail": "PaperTrail", "Waiting": "Waiting", "Scheduled": "Scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	unseen := 0
	for _, e := range slow {
		if !e.Seen {
			unseen++
		}
	}
	if len(uids) <= 50 && counts["Inbox"] != unseen {
		t.Errorf("STATUS unseen %d != counted %d", counts["Inbox"], unseen)
	}
}
```

Run: `go test ./... -run Hardening && make test-integration`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/imap/client.go internal/imap/memserver_test.go internal/integration_test.go
git commit -m "imap: pipeline SELECT with UID SEARCH / UID FETCH (one round trip fewer per folder load)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: Layer 1c — MOVE keeps the mailbox selected

**Files:**
- Modify: `internal/imap/client.go:1126` (`c.selectedMailbox = ""` in `MoveMessage`, the one after the COPYUID extraction — NOT the one inside the TRYCREATE branch)
- Test: `internal/imap/memserver_test.go`, `internal/integration_test.go`

- [ ] **Step 1: Write the failing test** (append to `memserver_test.go`)

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/imap -run TestMem_MoveMessage_KeepsSelectionAndBatchWorks -v`
Expected: FAIL `after MOVE selectedMailbox = "", want INBOX`.

- [ ] **Step 3: Implement**

In `MoveMessage`, delete the line `c.selectedMailbox = ""` that directly precedes `audit("MOVE %s uid=%d -> %s destUID=%d", ...)` and replace it with this comment:

```go
		// The source mailbox stays selected after MOVE (RFC 9051 §6.4.8); the
		// server's untagged EXPUNGE responses are consumed by go-imap and every
		// later operation is UID-addressed, so no re-SELECT is needed. Clearing
		// the cache here cost one extra round trip per MOVE.
```

Leave the `c.selectedMailbox = ""` inside the TRYCREATE branch untouched.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/imap && go vet ./internal/imap`
Expected: PASS.

- [ ] **Step 5: Live integration test** (append to `internal/integration_test.go`)

```go
// TestIntegration_MoveWithoutReselect pins that a MOVE followed by a header
// fetch on the same connection reflects the move on both sides without any
// forced re-SELECT (MoveMessage no longer clears the selection cache).
func TestIntegration_MoveWithoutReselect(t *testing.T) {
	env := loadEnv(t)
	cli := env.imapClient()
	defer cli.Close()
	testFolder := "NeomdTest"
	if _, err := cli.EnsureFolders(context.Background(), []string{testFolder}); err != nil {
		t.Fatalf("EnsureFolders: %v", err)
	}
	subject := uniqueSubject("move-noreselect")
	if err := smtp.Send(env.smtpConfig(), env.user, "", "", subject, "moved without re-select", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}
	email := waitForEmail(t, cli, "INBOX", subject, 30*time.Second)
	destUID, err := cli.MoveMessage(context.Background(), "INBOX", email.UID, testFolder)
	if err != nil {
		cleanupEmail(t, cli, "INBOX", email.UID)
		t.Fatalf("MoveMessage: %v", err)
	}
	src, err := cli.FetchHeaders(context.Background(), "INBOX", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range src {
		if e.UID == email.UID {
			t.Errorf("uid %d still listed in INBOX after MOVE", email.UID)
		}
	}
	moved := waitForEmail(t, cli, testFolder, subject, 10*time.Second)
	if destUID != 0 && moved.UID != destUID {
		t.Errorf("dest UID %d != COPYUID %d", moved.UID, destUID)
	}
	cleanupEmail(t, cli, testFolder, moved.UID)
}
```

Run: `make test-integration`
Expected: PASS including `TestIntegration_MoveWithoutReselect` and `TestIntegration_IMAPMoveAndUndo`.

- [ ] **Step 6: Hardening**

Run: `go test ./... -run Hardening`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/imap/client.go internal/imap/memserver_test.go internal/integration_test.go
git commit -m "imap: MOVE keeps the source mailbox selected (one round trip fewer per move)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: Layer 2 — background connection

**Files:**
- Modify: `cmd/neomd/main.go:64-125, 225-232`; `internal/ui/model.go` (struct ~line 544, helpers ~913, `fetchFolderCountsCmd` 2004, `bgFetchInboxCmd` 2041, `bgFetchVipFolderCmd` 2058, `bgExecAutoScreenCmd` 2130, `spyScanCmd` 1874, `checkOverdueScheduledCmd` 959, `emailsLoadedMsg` auto-screen condition ~2300)
- Test: `internal/ui/imap_client_helpers_test.go`, `internal/ui/instant_test.go` (new)

**Interfaces:**
- Produces: `(m Model) WithBackgroundClients(bg []*imap.Client) Model`, `(m Model) bgImapCli() *imap.Client`, field `bgClients []*imap.Client`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/ui/imap_client_helpers_test.go`:

```go
func TestBgImapCli_FallsBackToPrimaryWhenNil(t *testing.T) {
	primary := imap.New(imap.Config{Host: "h", Port: "993"})
	m := Model{clients: []*imap.Client{primary}, accountI: 0}
	if got := m.bgImapCli(); got != primary {
		t.Errorf("no bgClients: want primary, got %v", got)
	}
	m.bgClients = []*imap.Client{nil}
	if got := m.bgImapCli(); got != primary {
		t.Errorf("nil bg entry: want primary, got %v", got)
	}
	bg := imap.New(imap.Config{Host: "h", Port: "993"})
	m.bgClients = []*imap.Client{bg}
	if got := m.bgImapCli(); got != bg {
		t.Errorf("want bg client, got %v", got)
	}
}

func TestBgImapCli_NilWhenEverythingDisabled(t *testing.T) {
	m := Model{clients: []*imap.Client{nil}, bgClients: []*imap.Client{nil}}
	if got := m.bgImapCli(); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}
```

Create `internal/ui/instant_test.go`:

```go
package ui

// Workflow tests for the instant-IMAP layers: background connection,
// optimistic list updates and the per-folder cache. They drive the real
// bubbletea Update handlers with real messages; no network.

import (
	"testing"

	"github.com/sspaeti/neomd/internal/config"
	"github.com/sspaeti/neomd/internal/imap"
	"github.com/sspaeti/neomd/internal/screener"
)

// instantModel builds a one-account inbox model with n INBOX emails loaded
// (UIDs 1..n, newest = highest UID) and the cursor on the first row.
func instantModel(t *testing.T, n int) Model {
	t.Helper()
	cfg := &config.Config{Accounts: []config.AccountConfig{{Name: "P", From: "me@x"}}}
	cfg.Folders = config.FoldersConfig{Inbox: "INBOX", ToScreen: "ToScreen", Feed: "Feed", PaperTrail: "PaperTrail",
		Archive: "Archive", Waiting: "Waiting", Someday: "Someday", Scheduled: "Scheduled", Sent: "Sent", Trash: "Trash",
		ScreenedOut: "ScreenedOut", Drafts: "Drafts", Spam: "Spam"}
	dir := t.TempDir()
	sc, err := screener.New(screener.Config{
		ScreenedIn: dir + "/in.txt", ScreenedOut: dir + "/out.txt", Feed: dir + "/feed.txt",
		PaperTrail: dir + "/paper.txt", Spam: dir + "/spam.txt"})
	if err != nil {
		t.Fatal(err)
	}
	m := Model{cfg: cfg, accounts: cfg.ActiveAccounts(), folders: cfg.Folders.TabLabels(), screener: sc,
		inbox: newInboxList(120, 30, "Sent", "Drafts"), markedUIDs: map[uint32]bool{}, spyPixelKeys: map[string]bool{},
		sortField: "date", sortReverse: true, width: 120, height: 40}
	for i := n; i >= 1; i-- {
		e := mkEmail(uint32(i), "<m"+string(rune('0'+i))+"@x>", "s", "Sender <s@example.com>", n-i, true)
		e.Folder = "INBOX"
		m.emails = append(m.emails, e)
	}
	m.applyFilter()
	m.inbox.Select(0)
	return m
}

func uidsInList(m Model) []uint32 {
	var out []uint32
	for _, it := range m.inbox.Items() {
		out = append(out, it.(emailItem).email.UID)
	}
	return out
}

func TestInboxLoadSkipsAutoScreenWhileBgSyncRuns(t *testing.T) {
	m := instantModel(t, 2)
	// Sender is on the screened-out list, so a normal Inbox load would move it.
	if err := m.screener.Block("Sender <s@example.com>"); err != nil {
		t.Fatal(err)
	}
	m.bgSyncInProgress = true
	res, _ := m.Update(emailsLoadedMsg{emails: m.emails, folder: "INBOX"})
	mm := res.(Model)
	if mm.loading || mm.bulkProgress != nil {
		t.Errorf("auto-screen must be skipped while bg sync runs: loading=%v bulk=%v", mm.loading, mm.bulkProgress)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/ui -run 'TestBgImapCli|TestInboxLoadSkipsAutoScreen' -v`
Expected: FAIL, `m.bgClients undefined`, `m.bgImapCli undefined`.

- [ ] **Step 3: Implement**

`internal/ui/model.go`, struct: after `clients     []*imap.Client`:

```go
	bgClients   []*imap.Client         // second connection per account for housekeeping (nil = share primary)
```

After `imapCli()`:

```go
// bgImapCli returns the background IMAP connection for the active account,
// used for housekeeping that must never delay a user action (tab counts,
// 5-minute sync, spy scan, prefetch). Falls back to imapCli() when no
// background client exists, so every nil-client rule keeps holding.
func (m Model) bgImapCli() *imap.Client {
	if m.accountI < len(m.bgClients) && m.bgClients[m.accountI] != nil {
		return m.bgClients[m.accountI]
	}
	return m.imapCli()
}

// WithBackgroundClients attaches one background IMAP client per account
// (same order as clients; nil entries allowed).
func (m Model) WithBackgroundClients(bg []*imap.Client) Model {
	m.bgClients = bg
	return m
}
```

Replace `m.imapCli()` with `m.bgImapCli()` in exactly these functions: `fetchFolderCountsCmd`, `bgFetchInboxCmd` (both the `ResetMailboxSelection()` and `FetchHeaders` calls), `bgExecAutoScreenCmd`, `spyScanCmd` (`cli := m.bgImapCli()`). In `bgFetchVipFolderCmd` change `cli := m.imapCli()` to `cli := m.bgImapCli()`. In `checkOverdueScheduledCmd` keep `sentDraftsIMAPClient()` semantics but prefer the background one when the account is the active one:

```go
	cli := m.sentDraftsIMAPClient()
	if cli == m.imapCli() {
		cli = m.bgImapCli()
	}
```

`emailsLoadedMsg` auto-screen condition: change

```go
		if msg.folder == m.cfg.Folders.Inbox && m.cfg.UI.AutoScreen() && !m.screener.IsEmpty() {
```
to
```go
		// Skip while the 5-minute sync is mid-cycle: its MOVEs run on the
		// background connection and end with a refresh; a second MOVE for the
		// same mail would fail with a server NO.
		if msg.folder == m.cfg.Folders.Inbox && m.cfg.UI.AutoScreen() && !m.screener.IsEmpty() && !m.bgSyncInProgress {
```

`cmd/neomd/main.go`: after the loop that fills `imapClients` (line ~118), add

```go
	// Second connection per account for background housekeeping (tab counts,
	// 5-minute sync, spy scan, prefetch) so it never queues behind a user action.
	bgClients := make([]*goIMAP.Client, len(imapClients))
	for i, c := range imapClients {
		if c != nil {
			bgClients[i] = goIMAP.New(c.ConfigCopy())
		}
	}
	defer func() {
		for _, c := range bgClients {
			if c != nil {
				c.Close()
			}
		}
	}()
```

and change `model := ui.New(cfg, imapClients, sc, mailto)` to `model := ui.New(cfg, imapClients, sc, mailto).WithBackgroundClients(bgClients)`.

`internal/imap/client.go`, after `User()`:

```go
// ConfigCopy returns the connection config so a second client (background
// connection) can be built with identical credentials and TokenSource.
func (c *Client) ConfigCopy() Config { return c.cfg }
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/ui ./internal/imap && go vet ./... && go build ./...`
Expected: PASS.

- [ ] **Step 5: Smoke**

Run: `make build && NEOMD_IMAP_TRACE=1 ./neomd -config ~/.config/neomd-demo-hostpoint/config.toml`, press `Tab`, `Tab`, `q`. Then `grep -c FetchUnseenCounts ~/.cache/neomd/imap-trace.log`.
Expected: counts present; no error status in the TUI; both connections logged out cleanly (no panic on exit).

- [ ] **Step 6: Commit**

```bash
git add cmd/neomd/main.go internal/ui/model.go internal/imap/client.go internal/ui/imap_client_helpers_test.go internal/ui/instant_test.go
git commit -m "ui: background IMAP connection for tab counts, sync, spy scan

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Layer 3 — optimistic removal on move, delete, screen

**Files:**
- Modify: `internal/ui/model.go` — struct (add `refreshing bool` near `loading`), new helpers near `reselectEmail` (~1523), key handlers `x` (3276-3284), `I/O/F/P/$` (3327-3334), `A` (3337-3344), `B` (~3352-3359), `M*` chord (4012-4015), `batchDoneMsg` (2669-2693), `autoScreenDoneMsg` (2836-2847), `emailsLoadedMsg` auto-screen branch (~2307-2313), `bgScreenDoneMsg` (2893-2905), `viewInbox` (6302-6360)
- Test: `internal/ui/instant_test.go`

**Interfaces:**
- Produces: `(m *Model) removeFromList(targets []imap.Email) tea.Cmd`, `(m *Model) refreshActiveFolderCmd() tea.Cmd`, field `refreshing bool`.

- [ ] **Step 1: Write the failing tests** (append to `instant_test.go`)

```go
import tea "github.com/charmbracelet/bubbletea" // add to the import block

func key(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestOptimistic_DeleteRemovesRowImmediately(t *testing.T) {
	m := instantModel(t, 3) // list: 3,2,1 ; cursor on uid 3
	res, cmd := m.updateInbox(key("x"))
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("x must still fire the batch move command")
	}
	if mm.loading {
		t.Error("list must stay visible (loading=false)")
	}
	if got := uidsInList(mm); len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Errorf("list after x = %v, want [2 1]", got)
	}
	if e := selectedEmail(mm.inbox); e == nil || e.UID != 2 {
		t.Errorf("cursor should land on the next row (uid 2), got %+v", e)
	}
	if len(mm.markedUIDs) != 0 {
		t.Error("marks must be cleared")
	}
}

func TestOptimistic_BatchDoneSuccessRefreshesInBackground(t *testing.T) {
	m := instantModel(t, 3)
	res, _ := m.updateInbox(key("x"))
	mm := res.(Model)
	res, cmd := mm.Update(batchDoneMsg{undo: []undoMove{{uid: 9, fromFolder: "INBOX", toFolder: "Trash"}}})
	mm = res.(Model)
	if mm.loading {
		t.Error("success must not hide the list")
	}
	if !mm.refreshing || cmd == nil {
		t.Error("success must start a background refresh (refreshing=true, cmd!=nil)")
	}
	if len(mm.undoStack) != 1 || mm.undoStack[0][0].uid != 9 {
		t.Errorf("undo not pushed: %+v", mm.undoStack)
	}
	if mm.status != "Moved." {
		t.Errorf("status = %q", mm.status)
	}
}

func TestOptimistic_BatchErrorReloadsAndKeepsPartialUndo(t *testing.T) {
	m := instantModel(t, 3)
	res, _ := m.updateInbox(key("x"))
	mm := res.(Model)
	res, cmd := mm.Update(batchDoneMsg{err: errTest("MOVE 3 → Trash: NO"), undo: []undoMove{{uid: 5, fromFolder: "INBOX", toFolder: "Trash"}}})
	mm = res.(Model)
	if !mm.loading || cmd == nil {
		t.Error("an error must trigger a full server reload (loading=true, cmd!=nil)")
	}
	if !mm.isError || mm.status == "" {
		t.Error("error must be visible in the status line")
	}
	if len(mm.undoStack) != 1 || mm.undoStack[0][0].uid != 5 {
		t.Errorf("partial undo lost: %+v", mm.undoStack)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }

func TestOptimistic_ScreenInToScreenRemovesSameSender(t *testing.T) {
	m := instantModel(t, 4)
	m.activeFolderI = 1 // ToScreen tab
	for i := range m.emails {
		m.emails[i].Folder = "ToScreen"
	}
	m.emails[1].From = "Other <o@example.com>" // uid 3
	m.applyFilter()
	m.inbox.Select(0) // uid 4, Sender <s@example.com>
	res, cmd := m.updateInbox(key("I"))
	mm := res.(Model)
	if cmd == nil || mm.loading {
		t.Fatalf("I must fire the screener cmd and keep the list: cmd=%v loading=%v", cmd != nil, mm.loading)
	}
	if got := uidsInList(mm); len(got) != 1 || got[0] != 3 {
		t.Errorf("only the other sender should remain, got %v", got)
	}
}

func TestOptimistic_ScreenWithMarksRemovesOnlyTargets(t *testing.T) {
	m := instantModel(t, 3)
	m.markedUIDs[2] = true
	res, _ := m.updateInbox(key("O"))
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 2 || got[0] != 3 || got[1] != 1 {
		t.Errorf("list = %v, want [3 1]", got)
	}
}

func TestOptimistic_AutoScreenMovesHiddenBeforeFirstDraw(t *testing.T) {
	m := instantModel(t, 3)
	m.emails = nil
	m.applyFilter()
	if err := m.screener.Block("Sender <s@example.com>"); err != nil {
		t.Fatal(err)
	}
	blocked := mkEmail(7, "<b@x>", "spam", "Sender <s@example.com>", 0, false)
	blocked.Folder = "INBOX"
	ok := mkEmail(8, "<o@x>", "fine", "Friend <f@example.com>", 0, false)
	ok.Folder = "INBOX"
	res, cmd := m.Update(emailsLoadedMsg{emails: []imap.Email{ok, blocked}, folder: "INBOX"})
	mm := res.(Model)
	if cmd == nil {
		t.Fatal("moves must be fired")
	}
	if mm.loading {
		t.Error("list must not be hidden during auto-screen moves")
	}
	if got := uidsInList(mm); len(got) != 1 || got[0] != 8 {
		t.Errorf("blocked mail must be gone before first draw, list = %v", got)
	}
	res, _ = mm.Update(autoScreenDoneMsg{moved: 1, err: errTest("MOVE NO")})
	if !res.(Model).loading {
		t.Error("auto-screen error must reload from server")
	}
}

func TestOptimistic_BulkProgressShownInStatusNotSpinner(t *testing.T) {
	m := instantModel(t, 2)
	m.bulkProgress = &bulkOp{label: "Moving", total: 20}
	m.bulkProgress.moved.Store(3)
	out := m.View()
	if !strings.Contains(out, "Moving: 3/20") {
		t.Errorf("bulk progress missing from view:\n%s", out)
	}
	if strings.Contains(out, "Loading…") {
		t.Error("list must not be replaced by the loading spinner")
	}
}
```

Add `"strings"` and `tea "github.com/charmbracelet/bubbletea"` to the import block of `instant_test.go`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/ui -run 'TestOptimistic_' -v`
Expected: FAIL on list contents / `loading` / `refreshing`.

- [ ] **Step 3: Implement helpers** (after `reselectEmail`)

```go
// removeFromList drops targets (matched by folder+UID) from the visible list
// and rebuilds it. The cursor keeps its index so it lands on the next row,
// exactly as it does after today's post-move reload. Marks are cleared as a
// reload would. It never touches the server: the caller fires the same MOVE
// command as before, and any error there ends in a full reload.
func (m *Model) removeFromList(targets []imap.Email) tea.Cmd {
	gone := make(map[string]bool, len(targets))
	for _, e := range targets {
		gone[e.Folder+"\x00"+strconv.FormatUint(uint64(e.UID), 10)] = true
	}
	kept := m.emails[:0:0]
	for _, e := range m.emails {
		if !gone[e.Folder+"\x00"+strconv.FormatUint(uint64(e.UID), 10)] {
			kept = append(kept, e)
		}
	}
	m.emails = kept
	m.markedUIDs = make(map[uint32]bool)
	idx := m.inbox.Index()
	cmd := m.sortEmails()
	if n := len(m.inbox.Items()); idx >= n && n > 0 {
		idx = n - 1
	}
	if idx >= 0 {
		m.inbox.Select(idx)
	}
	return cmd
}

// sameSenderIn returns every loaded email from the same normalized sender
// as e in folder — mirrors the ToScreen sender expansion batchScreenerCmd
// performs on the server, so the list matches what the server will do.
func (m Model) sameSenderIn(e imap.Email, folder string) []imap.Email {
	s := normalizedSender(e.From)
	var out []imap.Email
	for _, x := range m.emails {
		if x.Folder == folder && normalizedSender(x.From) == s {
			out = append(out, x)
		}
	}
	return out
}

// refreshActiveFolderCmd re-fetches the visible folder without hiding the
// list: the cached rows stay, the header shows ↻ until emailsLoadedMsg.
func (m *Model) refreshActiveFolderCmd() tea.Cmd {
	m.refreshing = true
	return m.fetchFolderCmd(m.activeFolder())
}
```

Add `"strconv"` to the imports if missing. Struct: after `loading bool` add `refreshing bool // background re-fetch of the visible folder in flight (header shows ↻)`.

- [ ] **Step 4: Key handlers**

`x`:
```go
	case "x":
		targets := m.targetEmails()
		if len(targets) == 0 {
			return m, nil
		}
		m.bulkProgress = m.newBulkOp("Deleting", len(targets))
		m.status, m.isError = "Deleting…", false
		listCmd := m.removeFromList(targets)
		return m, tea.Batch(listCmd, m.spinner.Tick, m.batchMoveCmd(targets, m.cfg.Folders.Trash))
```

`A` (label "Archiving", `m.cfg.Folders.Archive`), `B` (label "Moving", `m.cfg.Folders.Work`) and the `M*` chord (`dst` from `dstMap`, label "Moving"): same shape — drop `m.loading = true`, set status, call `removeFromList`, batch the list cmd with the existing move cmd.

`I/O/F/P/$`:
```go
	case "I", "O", "F", "P", "$":
		targets := m.targetEmails()
		if len(targets) == 0 {
			return m, nil
		}
		m.bulkProgress = m.newBulkOp("Screening", len(targets))
		screenCmd := m.batchScreenerCmd(targets, key) // built BEFORE the list changes: it reads m.markedUIDs
		toRemove := targets
		if len(targets) == 1 && len(m.markedUIDs) == 0 && targets[0].Folder == m.cfg.Folders.ToScreen {
			toRemove = m.sameSenderIn(targets[0], m.cfg.Folders.ToScreen)
		}
		m.status, m.isError = "Screening…", false
		listCmd := m.removeFromList(toRemove)
		return m, tea.Batch(listCmd, m.spinner.Tick, screenCmd)
```

Note: `batchScreenerCmd` captures `m.markedUIDs` by value at construction, so it must be constructed before `removeFromList` clears the marks. The `x`/`A`/`M*` commands do not read marks, so order does not matter there.

- [ ] **Step 5: Message handlers**

`batchDoneMsg` — replace the tail after the undo push:

```go
		m.status = "Moved."
		m.isError = false
		return m, m.refreshActiveFolderCmd()
```
and in its error branch, before `return m, nil`, add the reload:
```go
			m.status = msg.err.Error()
			m.isError = true
			m.loading = true
			return m, tea.Batch(m.spinner.Tick, m.fetchFolderCmd(m.activeFolder()))
```

`autoScreenDoneMsg` — error branch adds the same reload (`m.loading = true` + fetch); success branch becomes `return m, m.refreshActiveFolderCmd()` instead of the spinner reload.

`emailsLoadedMsg` auto-screen branch — replace:
```go
			if moves := m.previewAutoScreen(); len(moves) > 0 {
				m.maybeNotifyInbox(msg.folder, msg.emails, moves)
				m.bulkProgress = m.newBulkOp("Screening", len(moves))
				moving := make([]imap.Email, len(moves))
				for i, mv := range moves {
					moving[i] = *mv.email
				}
				listCmd := m.removeFromList(moving) // hidden before the first draw; MOVEs run behind it
				return m, tea.Batch(listCmd, m.fetchFolderCountsCmd(), m.spinner.Tick, m.execAutoScreenCmd(moves))
			}
```
(The `sortCmd` computed earlier is superseded by `listCmd`; keep `sortCmd` for the other return paths.)

`bgScreenDoneMsg` — replace the spinner reload with `return m, m.refreshActiveFolderCmd()` when `msg.moved > 0`.

`emailsLoadedMsg` top: add `m.refreshing = false` next to `m.loading = false`.

`errMsg`: add `m.refreshing = false`.

- [ ] **Step 6: View**

In `viewInbox`, the `if m.loading {` branch stays. Change the status section so bulk progress is shown while the list is visible:

```go
	if m.cmdMode {
		...
	} else if bp := m.bulkProgress; bp != nil && !m.loading {
		b.WriteString(statusBar(bp.String(), false))
	} else if m.status != "" {
```

Add `↻` to the header after the tabs:

```go
	if m.refreshing {
		header += styleDate.Render(" ↻")
	}
	b.WriteString(header + "\n")
```

- [ ] **Step 7: Run tests**

Run: `go test ./internal/ui && go vet ./internal/ui`
Expected: PASS, including all pre-existing `internal/ui` tests. If `TestReloadKeepsCursorOnSameEmail` or another existing test asserts `loading == true` after a move key, update that assertion to the new optimistic contract and say so in the commit message.

- [ ] **Step 8: Hardening + live**

Run: `go test ./... -run Hardening && make test-integration`
Expected: PASS.

- [ ] **Step 9: Manual check on the demo account**

`make build && NEOMD_IMAP_TRACE=1 ./neomd -config ~/.config/neomd-demo-hostpoint/config.toml`: in ToScreen press `I` on a row, in Inbox press `x` then `u`, `A`. Every action must remove the row at once, show `↻` briefly, and `u` must bring the mail back. `tail ~/.cache/neomd/moves.log` shows one `MOVE` line per action.

- [ ] **Step 10: Commit**

```bash
git add internal/ui/model.go internal/ui/instant_test.go
git commit -m "ui: optimistic list updates for move/delete/screen; errors reload from server

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Layer 4 — per-folder cache with visible refresh

**Files:**
- Modify: `internal/config/config.go` (`UIConfig`, ~266-300), `internal/config/config_test.go`, `internal/ui/model.go` (struct; `fetchFolderCmd` 1014; `emailsLoadedMsg` 2245; switch sites 2215, 3496-3510, 3567, 3926, 3980; `R` 3582 and 4279 unchanged), `internal/ui/model_test.go:1412,1419`
- Test: `internal/ui/instant_test.go`

**Interfaces:**
- Produces: `folderSnapshot{emails []imap.Email; fetchedAt time.Time}`, `folderCache map[string]folderSnapshot`, `cacheKey(account, folder string) string`, `(m *Model) loadActiveFolder() tea.Cmd`, `emailsLoadedMsg.account string`, `UIConfig.InstantSwitch() bool`.

- [ ] **Step 1: Write the failing tests**

`internal/config/config_test.go` (append):

```go
func TestUIConfig_InstantSwitchDefaultsTrue(t *testing.T) {
	var u UIConfig
	if !u.InstantSwitch() {
		t.Error("default must be true")
	}
	f := false
	u.InstantFolderSwitch = &f
	if u.InstantSwitch() {
		t.Error("explicit false must disable")
	}
}
```

`internal/ui/instant_test.go` (append):

```go
func TestCache_SwitchToCachedFolderIsInstant(t *testing.T) {
	m := instantModel(t, 2)
	ts := mkEmail(50, "<ts@x>", "queued", "New <n@example.com>", 0, false)
	ts.Folder = "ToScreen"
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "ToScreen"): {emails: []imap.Email{ts}}}
	res, cmd := m.updateInbox(key("tab")) // Inbox -> ToScreen
	mm := res.(Model)
	if mm.loading {
		t.Error("cached folder must not show the spinner")
	}
	if !mm.refreshing || cmd == nil {
		t.Error("cached folder must still refresh in the background")
	}
	if got := uidsInList(mm); len(got) != 1 || got[0] != 50 {
		t.Errorf("list = %v, want cached [50]", got)
	}
}

func TestCache_SwitchToUncachedFolderShowsSpinner(t *testing.T) {
	m := instantModel(t, 2)
	res, cmd := m.updateInbox(key("tab"))
	mm := res.(Model)
	if !mm.loading || cmd == nil {
		t.Error("uncached folder keeps today's spinner path")
	}
}

func TestCache_LoadedResultIsCachedAndShownOnlyForActiveFolder(t *testing.T) {
	m := instantModel(t, 1)
	other := mkEmail(60, "<o@x>", "late", "X <x@example.com>", 0, false)
	other.Folder = "Archive"
	res, _ := m.Update(emailsLoadedMsg{emails: []imap.Email{other}, folder: "Archive", account: "P"})
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 1 || got[0] != 1 {
		t.Errorf("late result for another folder must not replace the list, got %v", got)
	}
	if snap, ok := mm.folderCache[cacheKey("P", "Archive")]; !ok || len(snap.emails) != 1 {
		t.Error("late result must still be cached")
	}
	mine := mkEmail(2, "<n@x>", "new", "X <x@example.com>", 0, false)
	mine.Folder = "INBOX"
	mm.refreshing = true
	res, _ = mm.Update(emailsLoadedMsg{emails: []imap.Email{mine}, folder: "INBOX", account: "P"})
	mm = res.(Model)
	if got := uidsInList(mm); len(got) != 1 || got[0] != 2 || mm.refreshing {
		t.Errorf("active folder result must be shown and clear ↻: list=%v refreshing=%v", got, mm.refreshing)
	}
}

func TestCache_LateResultForOtherFolderIsCachedNotShown(t *testing.T) {
	// Same guard, exercised through a real switch: user leaves INBOX before
	// its fetch returns.
	m := instantModel(t, 1)
	res, _ := m.updateInbox(key("tab")) // now ToScreen, loading
	mm := res.(Model)
	stale := mkEmail(9, "<s@x>", "stale", "X <x@example.com>", 0, false)
	stale.Folder = "INBOX"
	res, _ = mm.Update(emailsLoadedMsg{emails: []imap.Email{stale}, folder: "INBOX", account: "P"})
	mm = res.(Model)
	if !mm.loading {
		t.Error("ToScreen is still loading; an INBOX result must not end the spinner")
	}
	if len(uidsInList(mm)) != 0 {
		t.Errorf("INBOX rows must not appear under the ToScreen tab: %v", uidsInList(mm))
	}
}

func TestCache_KeyedByAccount(t *testing.T) {
	m := instantModel(t, 1)
	m.cfg.Accounts = append(m.cfg.Accounts, config.AccountConfig{Name: "W", From: "w@x"})
	m.accounts = m.cfg.ActiveAccounts()
	m.clients = []*imap.Client{imap.New(imap.Config{}), imap.New(imap.Config{})}
	foreign := mkEmail(70, "<w@x>", "work", "X <x@example.com>", 0, false)
	foreign.Folder = "INBOX"
	res, _ := m.Update(emailsLoadedMsg{emails: []imap.Email{foreign}, folder: "INBOX", account: "W"})
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 1 || got[0] != 1 {
		t.Errorf("account W's INBOX must not show under account P, got %v", got)
	}
	if _, ok := mm.folderCache[cacheKey("W", "INBOX")]; !ok {
		t.Error("cached under account W")
	}
	if _, ok := mm.folderCache[cacheKey("P", "INBOX")]; ok {
		t.Error("must not be cached under account P")
	}
}

func TestCache_RefreshKeyBypassesCache(t *testing.T) {
	m := instantModel(t, 1)
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "INBOX"): {emails: m.emails}}
	res, cmd := m.updateInbox(key("R"))
	mm := res.(Model)
	if !mm.loading || cmd == nil {
		t.Error("R is the hard refresh: spinner path")
	}
}

func TestCache_DisabledByConfigUsesSpinner(t *testing.T) {
	m := instantModel(t, 2)
	f := false
	m.cfg.UI.InstantFolderSwitch = &f
	ts := mkEmail(50, "<ts@x>", "queued", "New <n@example.com>", 0, false)
	ts.Folder = "ToScreen"
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "ToScreen"): {emails: []imap.Email{ts}}}
	res, _ := m.updateInbox(key("tab"))
	if !res.(Model).loading {
		t.Error("instant_folder_switch=false must restore the spinner path")
	}
}

func TestCache_HeaderShowsRefreshMarker(t *testing.T) {
	m := instantModel(t, 1)
	if strings.Contains(m.View(), "↻") {
		t.Error("no ↻ when not refreshing")
	}
	m.refreshing = true
	if !strings.Contains(m.View(), "↻") {
		t.Error("↻ expected while refreshing")
	}
}
```

Add `"strings"` to the imports. `key("tab")` must produce the same `tea.KeyMsg` the real program sends: use `tea.KeyMsg{Type: tea.KeyTab}` for it — add `func keyTab() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyTab} }` and use `keyTab()` in the tests above where `key("tab")` appears.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/ui -run 'TestCache_' -v; go test ./internal/config -run TestUIConfig_InstantSwitch`
Expected: FAIL, `undefined: folderSnapshot`, `cacheKey`, `InstantFolderSwitch`.

- [ ] **Step 3: Config**

`UIConfig` add:
```go
	InstantFolderSwitch   *bool           `toml:"instant_folder_switch"`   // show the last-seen list at once and refresh behind it (default true)
```
and:
```go
// InstantSwitch reports whether folder switches show the cached list first
// (default true). false restores the spinner-on-every-switch behavior.
func (u UIConfig) InstantSwitch() bool {
	if u.InstantFolderSwitch == nil {
		return true
	}
	return *u.InstantFolderSwitch
}
```

- [ ] **Step 4: Model**

Types (next to `autoScreenMove`):
```go
// folderSnapshot is the last header list fetched for one account+folder.
type folderSnapshot struct {
	emails    []imap.Email
	fetchedAt time.Time
}

func cacheKey(account, folder string) string { return account + "\x00" + folder }
```

Struct, after `emails  []imap.Email`:
```go
	folderCache map[string]folderSnapshot // last-seen list per account+folder; shown at once on switch with ↻ while re-fetching
	prefetched  bool                      // startup prefetch already scheduled
```

`emailsLoadedMsg` type: add `account string`. `fetchFolderCmd`:
```go
func (m Model) fetchFolderCmd(folder string) tea.Cmd {
	account := m.activeAccount().Name
	return func() tea.Msg {
		emails, err := m.imapCli().FetchHeaders(nil, folder, m.cfg.UI.InboxCount)
		if err != nil {
			return errMsg{err}
		}
		return emailsLoadedMsg{emails: emails, folder: folder, account: account}
	}
}
```

New helper (near `refreshActiveFolderCmd`):
```go
// loadActiveFolder is what every tab switch calls. With a cached snapshot
// (and instant_folder_switch on) the list appears at once and the header
// shows ↻ until the fresh fetch lands; otherwise the spinner path as before.
func (m *Model) loadActiveFolder() tea.Cmd {
	folder := m.activeFolder()
	if m.cfg.UI.InstantSwitch() {
		if snap, ok := m.folderCache[cacheKey(m.activeAccount().Name, folder)]; ok {
			m.emails = snap.emails
			m.markedUIDs = make(map[uint32]bool)
			m.filterActive, m.filterText = false, ""
			m.loading = false
			listCmd := m.sortEmails()
			return tea.Batch(listCmd, m.refreshActiveFolderCmd())
		}
	}
	m.loading = true
	return tea.Batch(m.spinner.Tick, m.fetchFolderCmd(folder))
}
```

Replace `m.loading = true; return m, tea.Batch(m.spinner.Tick, m.fetchFolderCmd(m.activeFolder()))` with `return m, m.loadActiveFolder()` at exactly these sites: mouse tab click (2215-2219), `tab`/`L`/`]` (3496-3502), `shift+tab`/`H`/`[` (3504-3510), account switch `ctrl+a` (3567-3569), leader digit `1`-`9` (3926-3929), `g<letter>` folder map (3980-3984). Leave `R` (3582, 4279), `gS`, `gd`, undo, error paths and all `fetchFolderCmd` calls inside message handlers as they are.

`emailsLoadedMsg` handler — new head:
```go
	case emailsLoadedMsg:
		if m.folderCache == nil {
			m.folderCache = make(map[string]folderSnapshot)
		}
		m.folderCache[cacheKey(msg.account, msg.folder)] = folderSnapshot{emails: msg.emails, fetchedAt: time.Now()}
		// Apply to the visible list only if the user is still on this folder
		// of this account; a late result for another folder is cached only.
		if msg.folder != m.activeFolder() || (msg.account != "" && msg.account != m.activeAccount().Name) {
			return m, nil
		}
		m.loading = false
		m.refreshing = false
		prevCursor := selectedEmail(m.inbox)
		... (rest unchanged)
```

`removeFromList` (Task 7) also updates the cache so a cached folder never re-shows a removed row:
```go
	if snap, ok := m.folderCache[cacheKey(m.activeAccount().Name, m.activeFolder())]; ok {
		keptSnap := snap.emails[:0:0]
		for _, e := range snap.emails {
			if !gone[e.Folder+"\x00"+strconv.FormatUint(uint64(e.UID), 10)] {
				keptSnap = append(keptSnap, e)
			}
		}
		snap.emails = keptSnap
		m.folderCache[cacheKey(m.activeAccount().Name, m.activeFolder())] = snap
	}
```

`bgInboxFetchedMsg`: after the nil check, cache the fresh Inbox: `m.folderCache[cacheKey(m.activeAccount().Name, m.cfg.Folders.Inbox)] = folderSnapshot{emails: msg.emails, fetchedAt: time.Now()}` (create the map if nil).

`model_test.go:1412,1419`: change `folder: "OTHER"` to `folder: "INBOX"` (the test's intent is a reload of the active folder).

- [ ] **Step 5: Run tests**

Run: `go test ./internal/ui ./internal/config && go vet ./...`
Expected: PASS.

- [ ] **Step 6: Manual check**

`make build && ./neomd -config ~/.config/neomd-demo-hostpoint/config.toml`: `Tab` to ToScreen (spinner once), `Tab` around the ring and back: every revisit shows the list at once with `↻` for a moment. `R` shows the spinner. `ctrl+a` (if two accounts) never shows the other account's rows.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/ui/model.go internal/ui/model_test.go internal/ui/instant_test.go
git commit -m "ui: per-folder cache — switches show the last list at once, ↻ while re-fetching

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Layer 4 — startup prefetch on the background connection

**Files:**
- Modify: `internal/ui/model.go` (message types ~136; `emailsLoadedMsg` tail; new cmd near `bgFetchInboxCmd`)
- Test: `internal/ui/instant_test.go`

**Interfaces:**
- Produces: `folderPrefetchedMsg{account, folder string; emails []imap.Email; remaining []string}`, `(m Model) prefetchFoldersCmd(remaining []string) tea.Cmd`, `(m Model) prefetchList(exclude string) []string`.

- [ ] **Step 1: Write the failing tests**

```go
func TestPrefetch_ListStartsWithToScreenAndSkipsCachedAndActive(t *testing.T) {
	m := instantModel(t, 1)
	m.folderCache = map[string]folderSnapshot{cacheKey("P", "Archive"): {}}
	got := m.prefetchList("INBOX")
	if len(got) == 0 || got[0] != "ToScreen" {
		t.Fatalf("ToScreen must be first, got %v", got)
	}
	for _, f := range got {
		if f == "INBOX" || f == "Archive" {
			t.Errorf("active/cached folder %q must be skipped", f)
		}
	}
}

func TestPrefetch_MsgFillsCacheOnlyAndChains(t *testing.T) {
	m := instantModel(t, 1)
	ts := mkEmail(50, "<ts@x>", "queued", "New <n@example.com>", 0, false)
	ts.Folder = "ToScreen"
	res, cmd := m.Update(folderPrefetchedMsg{account: "P", folder: "ToScreen", emails: []imap.Email{ts}, remaining: []string{"Feed"}})
	mm := res.(Model)
	if got := uidsInList(mm); len(got) != 1 || got[0] != 1 {
		t.Errorf("prefetch must never touch the visible list, got %v", got)
	}
	if snap, ok := mm.folderCache[cacheKey("P", "ToScreen")]; !ok || len(snap.emails) != 1 {
		t.Error("prefetched folder not cached")
	}
	if cmd == nil {
		t.Error("remaining folders must be chained")
	}
	res, cmd = mm.Update(folderPrefetchedMsg{account: "P", folder: "Feed", emails: nil, remaining: nil})
	if cmd != nil {
		t.Error("chain ends when remaining is empty")
	}
}

func TestPrefetch_ScheduledOnceAfterFirstLoad(t *testing.T) {
	m := instantModel(t, 1)
	res, cmd := m.Update(emailsLoadedMsg{emails: m.emails, folder: "INBOX", account: "P"})
	mm := res.(Model)
	if !mm.prefetched || cmd == nil {
		t.Error("first load must schedule the prefetch")
	}
	f := false
	mm.cfg.UI.InstantFolderSwitch = &f
	mm.prefetched = false
	res, _ = mm.Update(emailsLoadedMsg{emails: m.emails, folder: "INBOX", account: "P"})
	if res.(Model).prefetched {
		t.Error("no prefetch when instant_folder_switch is off")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/ui -run 'TestPrefetch_' -v`
Expected: FAIL, undefined symbols.

- [ ] **Step 3: Implement**

Message type (next to `bgInboxFetchedMsg`):
```go
	// folderPrefetchedMsg carries one background-fetched folder for the cache
	// only (never the visible list) and the folders still to prefetch.
	folderPrefetchedMsg struct {
		account   string
		folder    string
		emails    []imap.Email // nil on error; the folder simply stays uncached
		remaining []string
	}
```

Commands (near `bgFetchInboxCmd`):
```go
// prefetchList returns the IMAP names of every tab folder that is neither
// exclude nor already cached, ToScreen first (the most common first switch).
func (m Model) prefetchList(exclude string) []string {
	account := m.activeAccount().Name
	var out []string
	add := func(f string) {
		if f == "" || f == exclude {
			return
		}
		if _, ok := m.folderCache[cacheKey(account, f)]; ok {
			return
		}
		for _, x := range out {
			if x == f {
				return
			}
		}
		out = append(out, f)
	}
	add(m.cfg.Folders.ToScreen)
	for _, label := range m.folders {
		add(folderLabelToIMAP(label, m.cfg.Folders))
	}
	return out
}

// prefetchFoldersCmd fetches remaining[0] on the background connection and
// hands the rest back through folderPrefetchedMsg so folders load one after
// another, never in parallel with a user action on the primary connection.
func (m Model) prefetchFoldersCmd(remaining []string) tea.Cmd {
	if len(remaining) == 0 {
		return nil
	}
	cli := m.bgImapCli()
	account := m.activeAccount().Name
	folder, rest := remaining[0], remaining[1:]
	n := m.cfg.UI.InboxCount
	return func() tea.Msg {
		if cli == nil {
			return folderPrefetchedMsg{account: account, folder: folder, remaining: rest}
		}
		emails, err := cli.FetchHeaders(nil, folder, n)
		if err != nil {
			return folderPrefetchedMsg{account: account, folder: folder, remaining: rest}
		}
		return folderPrefetchedMsg{account: account, folder: folder, emails: emails, remaining: rest}
	}
}
```

Handler (next to `bgInboxFetchedMsg`):
```go
	case folderPrefetchedMsg:
		if msg.emails != nil {
			if m.folderCache == nil {
				m.folderCache = make(map[string]folderSnapshot)
			}
			if _, already := m.folderCache[cacheKey(msg.account, msg.folder)]; !already {
				m.folderCache[cacheKey(msg.account, msg.folder)] = folderSnapshot{emails: msg.emails, fetchedAt: time.Now()}
			}
		}
		return m, m.prefetchFoldersCmd(msg.remaining)
```
(`!already` keeps a fresher user-driven load from being overwritten by an older prefetch result.)

`emailsLoadedMsg`: right before the final `return m, tea.Batch(sortCmd, m.fetchFolderCountsCmd())` (and inside the auto-screen and notify branches' returns too — simplest: compute once after the folder/account guard)
```go
		var prefetchCmd tea.Cmd
		if !m.prefetched && m.cfg.UI.InstantSwitch() {
			m.prefetched = true
			prefetchCmd = m.prefetchFoldersCmd(m.prefetchList(msg.folder))
		}
```
and add `prefetchCmd` to every `tea.Batch(...)` returned by this handler.

Verify `folderLabelToIMAP(label, m.cfg.Folders)` exists at `model.go:2095` with that signature; if its parameter order differs, adapt the call.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/ui && go vet ./...`
Expected: PASS.

- [ ] **Step 5: Manual check**

`make build && NEOMD_IMAP_TRACE=1 ./neomd -config ~/.config/neomd-demo-hostpoint/config.toml`, wait 3 s, `Tab`: ToScreen appears with no spinner and `↻` briefly. `grep FetchHeaders ~/.cache/neomd/imap-trace.log | tail -12` shows one line per tab folder after the first Inbox line.

- [ ] **Step 6: Commit**

```bash
git add internal/ui/model.go internal/ui/instant_test.go
git commit -m "ui: prefetch tab folders on the background connection after first load

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: Docs, after-numbers, final verification

**Files:**
- Modify: `AGENTS.md` (sections *IMAP & Runtime Resilience* ~429-466 and *Inbox Display* ~367-393), `CHANGELOG.md` (top), `docs/content/docs/configuration/_index.md` (~line 88), `README.md` (Benchmark section ~357), `docs/superpowers/plans/2026-09-30-instant-imap.md` (`## Baseline numbers`)

- [ ] **Step 1: After-numbers**

Run the same script as Task 0 step 4 twice with `NEOMD_IMAP_TRACE=1 make demo-hp`: switch Inbox→ToScreen→Inbox→ToScreen, `I` one row, `x` one row in Inbox, `A` one row. Read `~/.cache/neomd/imap-trace.log` and fill the table under `## Baseline numbers`:

```
| Action (demo-hp)           | Before (RTTs / felt) | After (RTTs before redraw / felt) |
|----------------------------|----------------------|-----------------------------------|
| Switch, seen before        | 3 / …ms              | 0 / 0 ms (↻ …ms)                   |
| Switch, first time         | 3 / …ms              | 2 / …ms                            |
| Screen-in I                | ~10 / …ms            | 0 / 0 ms                           |
| Delete x                   | 9 / …ms              | 0 / 0 ms                           |
```

- [ ] **Step 2: AGENTS.md**

Under *IMAP & Runtime Resilience* add:

```markdown
- **SELECT is pipelined with the first command; MOVE keeps the mailbox selected** —
  `beginSelect`/`endSelect` (`internal/imap/client.go`) send SELECT and the following
  UID SEARCH / UID FETCH back-to-back (RFC 9051 §5.5); a failed SELECT drains the
  pipelined response and the connection stays usable. `FetchUnseenCounts` sends all
  STATUS commands before waiting. `MoveMessage` no longer clears `selectedMailbox`
  (RFC 9051 §6.4.8: the source stays selected; every later op is UID-addressed).
  Never reintroduce a per-command SELECT. Tests: `TestMem_FetchHeaders_*`,
  `TestMem_FetchUnseenCounts`, `TestMem_MoveMessage_KeepsSelectionAndBatchWorks`,
  `TestIntegration_MoveWithoutReselect`.
- **Background connection is never used for user actions** — `bgImapCli()` serves
  tab counts, the 5-minute sync, VIP polls, spy scan, overdue check and prefetch;
  folder loads, body fetches, moves, screening, flags, search, undo and the
  auto-screen MOVEs after an Inbox load stay on `imapCli()` so a user's consecutive
  actions are serial. Falls back to the primary when nil. While `bgSyncInProgress`
  an Inbox load skips its own auto-screen pass. Tests: `TestBgImapCli_*`,
  `TestInboxLoadSkipsAutoScreenWhileBgSyncRuns`.
- **`NEOMD_IMAP_TRACE=1`** appends `<time> <op> <ms>` per IMAP operation to
  `~/.cache/neomd/imap-trace.log` (`imap.SetTracePath`, `config.IMAPTracePath`). First
  stop for any "neomd feels slow" report; the number of lines per keypress is the
  round-trip count. Tests: `TestTrace_*`.
```

Under *Inbox Display* add:

```markdown
- **Optimistic removal is never trusted past an error** — `x`/`A`/`B`/`M*`/`I O F P $`
  and auto-screen drop rows via `removeFromList` before the MOVE runs; success ends in
  a background refresh (`refreshActiveFolderCmd`, header `↻`), any error ends in
  `loading = true` + full reload with the error in the status line and partial undo
  kept. Server calls, order and audit lines are unchanged. `u` undo keeps the spinner
  reload. Tests: `TestOptimistic_*`.
- **A cached list is only shown with `↻` and a fetch in flight** — `loadActiveFolder`
  serves `folderCache[account+folder]` on tab switches (`[ui].instant_folder_switch`,
  default true); `emailsLoadedMsg` always caches and applies to the visible list only
  when folder AND account are still active (late results for another folder are cached,
  not shown). `R` bypasses the cache. Prefetch (`folderPrefetchedMsg`) fills the cache
  only. Tests: `TestCache_*`, `TestPrefetch_*`.
```

- [ ] **Step 3: CHANGELOG.md** — new `# 2026-09-30` section at the top (one bullet per layer, bold title, what/why/where, test names, and the before/after table from step 1). Follow the exact style of the `# 2026-09-18` safety entry.

- [ ] **Step 4: Config docs** — in `docs/content/docs/configuration/_index.md` after the `bulk_progress_threshold` line add:

```toml
instant_folder_switch = true    # show the last-seen list at once on Tab and refresh behind it (↻); false = spinner on every switch
```

and a short paragraph under the `[ui]` section explaining `NEOMD_IMAP_TRACE=1` and `~/.cache/neomd/imap-trace.log`.

- [ ] **Step 5: README.md** — in *Benchmark*, after the provider tables, add the "Round trips after" table from the spec (section *Round trips after*) with one sentence: what matters is round trips per action times your provider's latency.

- [ ] **Step 6: Full verification**

Run: `make fmt-check && go vet ./... && go test ./... && go test ./... -run Hardening && make test-integration && make docs`
Expected: all PASS; `make docs` regenerates keybindings unchanged (no key changes) and syncs README.

- [ ] **Step 7: Commit**

```bash
git add AGENTS.md CHANGELOG.md README.md docs/content docs/superpowers/plans/2026-09-30-instant-imap.md
git commit -m "docs: instant IMAP — invariants, changelog with before/after, config knob

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

- [ ] **Step 8: Production soak before merge**

Run neomd on the production config with `NEOMD_IMAP_TRACE=1` for one working day. Before merging `speed-improvements` into `dev`, check: `moves.log` has exactly one MOVE line per action taken; `imap-trace.log` shows no errors; no `MOVE-FAILED` lines; `u` undo worked every time it was used.
