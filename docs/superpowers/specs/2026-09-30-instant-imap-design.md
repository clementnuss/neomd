# Instant folder switch and screening — fewer IMAP round trips

Date: 2026-09-30

## Problem

neomd's promise is that every action feels instant. Measured on 2026-09-30
(Hostpoint demo account, Wi-Fi link saturated by a backup job, NOOP round
trip 250–450 ms), a folder switch took ~1 s and a screen-in 1–2 s. The
same actions on an idle link (18 ms round trip) take 60–200 ms.

The UI is not the cost. Sorting, threading and a full `View()` render of
200 emails each take under 1 ms. `inbox_count` does not matter either (the
demo Inbox holds 22 messages and shows the same latency).

The cost is the number of **sequential IMAP round trips** per action, all
on one mutex-guarded connection:

| Action | Round trips today | Where |
|---|---|---|
| Folder switch (`Tab`, `gk`, …) | SELECT, UID SEARCH, FETCH = **3**, then 4 serial STATUS for tab counts = **4 more** holding the connection | `FetchHeaders`, `FetchUnseenCounts` |
| Screen-in `I` on one ToScreen row | UID SEARCH + FETCH (sender expansion) = 2, MOVE = 1, forced re-SELECT + UID SEARCH + FETCH reload = 3, STATUS ×4 = 4 → **~10** | `batchScreenerCmd`, `MoveMessage`, `emailsLoadedMsg` |
| Delete / archive / move (`x`, `A`, `M*`) | MOVE ×N (+1 re-SELECT each after the first), reload 3, STATUS 4 → **2N + 7** | `batchMoveCmd`, `moveDoneMsg` |

Every round trip is paid for by the user's next keypress, because the
STATUS calls and reloads queue on the same connection. Nothing was added
recently that made this worse; the structure dates from March and the
network got slower. But the structure is what multiplies network latency
into seconds.

Measured on the same connection, same network, 2026-09-30 (round 0):

| Operation | Serial (today) | Pipelined |
|---|---|---|
| 4× STATUS | 1114 ms | 204 ms |
| Switch Inbox: SELECT+SEARCH+FETCH | 883 ms | 517 ms |
| Switch ToScreen | 1238 ms | 655 ms |

## Goals

1. Folder switch shows the list in **0 ms** when the folder was seen
   before, and in **2 round trips** otherwise.
2. Screen-in, delete, archive and move update the list **immediately**;
   the server work runs behind the list.
3. Background housekeeping (tab counts, 5-minute sync, spy scan) never
   delays a user action.
4. No capability changes. Same keys, same folders, same screener lists,
   same undo, same audit log, same threading, same merges, same
   notifications, same headless daemon.
5. **No email is ever lost or silently misplaced.** Every server operation
   is exactly the one issued today; only *when the list is redrawn*
   changes. Every error still ends in a full reload from the server, with
   the error in the status line.

## Non-goals

- IMAP IDLE / push. The 5-minute sync stays.
- Local mail store, offline mode, body caching.
- Changing what `FetchHeaders` fetches per message (envelope, flags, size,
  extended BODYSTRUCTURE, `X-Neomd-Send-At` peek stay; measured no
  meaningful cost).
- Skipping the ToScreen sender expansion (kept as-is).
- Reducing round trips of `u` undo (rare; stays a spinner reload).

## Design

Four independent layers, ordered by risk. Each is its own commit and can
be reverted alone.

### Layer 0 — IMAP timing trace (measurement, opt-in)

`NEOMD_IMAP_TRACE=1` makes `internal/imap` append one line per public
operation to `config.IMAPTracePath()` (next to `moves.log`):

```
2026-09-30T10:12:03.412+02:00 FetchHeaders INBOX n=200 count=22 187ms
2026-09-30T10:12:03.601+02:00 FetchUnseenCounts 4 folders 41ms
2026-09-30T10:12:05.010+02:00 MoveMessage INBOX uid=4123 -> Archive 39ms
```

Implemented with a `defer trace(...)` at the top of `FetchHeaders`,
`FetchHeadersByUID`, `SearchUIDs`, `FetchUnseenCounts`, `FetchBody`,
`MoveMessage`, `ExpungeAll`, `searchFolder`, `MarkSeen`/`MarkUnseen`.
Off by default: zero cost, no file. This is how the before/after numbers
in this spec are reproduced, and how the next slowdown gets diagnosed
without guessing.

### Layer 1 — Pipelining and one fewer SELECT (no behavior change)

go-imap v2 sends a command as soon as its method is called and only
blocks in `Wait()`. RFC 9051 §5.5 allows a client to send the next
command before the previous response arrives as long as the second does
not depend on the first's *result*. UID SEARCH ALL and STATUS depend on
nothing the client has to read first.

**1a. `FetchUnseenCounts`** issues all STATUS commands, then waits for
each. Error handling unchanged: a network error aborts and lets
`withConnRetry` reconnect; a per-folder NO (missing mailbox) is skipped.
4 round trips → 1.

**1b. SELECT + first command pipelined.** A small helper pair in
`client.go`:

```go
// beginSelect sends SELECT when folder is not the cached selection and
// returns the in-flight command (nil when no SELECT was needed).
func (c *Client) beginSelect(conn *imapclient.Client, folder string) *imapclient.SelectCommand
// endSelect waits for a beginSelect command and records the selection.
func (c *Client) endSelect(cmd *imapclient.SelectCommand, folder string) error
```

Used in `FetchHeaders` (SELECT ‖ UID SEARCH, then FETCH: 3 → 2 round
trips), `SearchUIDs` (2 → 1), `FetchHeadersByUID` (SELECT ‖ UID FETCH:
2 → 1) and `searchFolder` (2 → 1 per folder; `space /` across 11 folders
saves 11 round trips). If SELECT fails, the pipelined command's response
is drained and the SELECT error is returned, exactly as today.
`selectedMailbox` is only recorded after SELECT succeeded.

**1c. MOVE keeps the mailbox selected.** `MoveMessage` currently clears
`selectedMailbox` after every successful MOVE ("mailbox state changes
after move", first version, 2026-03-24). Nothing reads that field except
`selectMailbox`, whose only effect is whether a redundant SELECT is sent.
The clear was defensive: RFC 9051 §6.4.8 says the source mailbox stays
selected after MOVE and the server sends untagged EXPUNGE for the moved
messages, which go-imap consumes. Every operation neomd runs afterwards
is UID-addressed (UID SEARCH, UID FETCH, UID MOVE, UID STORE), so
sequence-number shifts cannot misaddress anything. Removing the clear
saves 1 round trip per MOVE after the first in a batch (10 marked
emails: 20 → 11 round trips). The TRYCREATE path (destination folder
missing) keeps its explicit re-SELECT. `ExpungeAll` is untouched.

Pinned by an in-memory-server test (Layer T1) and a live integration
test: MOVE then `FetchHeaders(src)` no longer lists the UID and
`FetchHeaders(dst)` does, with no SELECT in between.

### Layer 2 — A second connection for background work

`cmd/neomd/main.go` builds a second `*imap.Client` per account with the
same config (`bgClients`, nil for `imap_disabled` accounts, same
`TokenSource` for OAuth2). `Model.bgImapCli()` returns it for the active
account and **falls back to `imapCli()` when nil**, so every existing
nil-client rule keeps holding. Both are closed on exit. The headless
daemon is untouched.

Moved to the background client: `fetchFolderCountsCmd`,
`bgFetchInboxCmd`, `bgFetchVipFolderCmd`, `bgExecAutoScreenCmd`,
`spyScanCmd`, `checkOverdueScheduledCmd` and the Layer 4 prefetch.

Everything the user triggers directly (folder load, body fetch, move,
delete, screen, flags, search, undo) stays on the primary connection so
ordering between a user's consecutive actions is unchanged. The
auto-screen MOVEs that follow an Inbox load (`execAutoScreenCmd`) also
stay on the primary connection: they must be serial with the user's next
folder fetch, otherwise a refresh could list mail that is mid-move and
the following load would try to screen it again.

While the 5-minute sync is mid-cycle (`bgSyncInProgress`), an Inbox load
skips its own auto-screen pass: the sync's MOVEs run on the background
connection and end with a refresh of the visible folder, so a concurrent
load must not issue a second MOVE for the same mail (which would fail
with a server NO and surface as an error).

Consequence to be aware of: a MOVE on the background connection while
the primary has the same folder selected produces untagged EXPUNGE on
the primary's next command. go-imap handles it; all neomd operations are
UID-based, so nothing is misaddressed. This is the same situation as a
phone client moving mail while neomd is open, which already works.

The connection lazily opens on first use, so startup does not wait for
it. Two connections per account are well inside every provider's limit
(Gmail 15, Office365 20, Dovecot/Hostpoint unlimited by default).

### Layer 3 — Optimistic list updates for move, delete, screen

Today every `x`, `A`, `B`, `M*`, `I/O/F/P/$` and auto-screen sets
`loading = true` (list hidden, spinner), runs the MOVEs, then reloads the
folder (3 round trips) and then the tab counts (4 round trips).

New flow, one helper in `model.go`:

```go
// removeFromList drops targets (matched by folder+UID) from the visible
// list and from the folder cache, clears marks, rebuilds the list so the
// cursor lands on the next row exactly as it does after today's reload,
// and returns the list command. It never touches the server.
func (m *Model) removeFromList(targets []imap.Email) tea.Cmd
```

Key handlers call `removeFromList`, set the status line to the bulk
label ("Moving 3…", "Screening…"), and fire the **same** `batchMoveCmd`
/ `batchScreenerCmd` as today. `m.loading` stays false: the list is
visible and usable at once.

- **Success** (`batchDoneMsg`/`autoScreenDoneMsg` without error): status
  "Moved." / "Screened N", undo entries pushed exactly as today, **no
  spinner reload**. Instead a *background refresh* of the active folder
  (Layer 4 path: list stays, `↻` shown) so newly arrived mail still
  appears after an action, as it does today.
- **Error** (any MOVE failed, rollback ran or not): status shows the
  error, `m.loading = true`, **full reload from the server** — today's
  behavior. Partial undo info is kept as today. The server is the source
  of truth after any failure; the optimistic removal is never trusted
  past an error.
- **Screener sender expansion**: when a single unmarked row is screened
  from ToScreen, the server side also moves every queued mail from that
  sender. The optimistic removal mirrors that locally by removing every
  loaded row with the same `normalizedSender`, so the list matches what
  the server does. Rows beyond `inbox_count` were never displayed and
  are reconciled by the background refresh.
- **Auto-screen on Inbox load**: `previewAutoScreen()` already knows the
  moves before display. The moved emails are removed from the list
  before the first draw, the MOVEs run on the background connection, and
  the list is never hidden. Error → full reload, as today.
- **Bulk progress**: `bulkProgress` (n/total) is rendered in the status
  line while it is non-nil instead of in the hidden-list spinner branch.
  Same information, list stays visible.
- **Undo** (`u`) is unchanged: restored emails are not known locally, so
  it keeps the spinner reload.
- **Reader-view actions** that move the open email go through the same
  helper when they return to the inbox.

Server-side calls, order, audit-log lines, undo UIDs, screener list
writes and rollbacks are byte-for-byte what they are today. Only the
redraw timing changes.

### Layer 4 — Per-folder cache with visible refresh

```go
type folderSnapshot struct {
    emails    []imap.Email
    fetchedAt time.Time
}
// key: account name + "\x00" + IMAP folder name
folderCache map[string]folderSnapshot
refreshing  bool // background refresh of the visible folder in flight
```

**Switch.** Every place that changes the active folder and today does
`m.loading = true; fetchFolderCmd(...)` calls `m.loadActiveFolder()`:

- cache hit → `m.emails` = snapshot, list rebuilt, `loading = false`,
  `refreshing = true`, and the same `fetchFolderCmd` is fired. The user
  sees the folder at once and can act on it.
- cache miss → today's behavior (spinner).

**Load result.** `emailsLoadedMsg` carries the account name and folder.
It always writes the snapshot into the cache. It is applied to the
visible list **only if** the folder and account are still the active
ones; otherwise it is cached and dropped from display. This guard also
closes a latent race that exists today (a late result for folder A
overwriting the list after the user switched to B). On apply,
`refreshing = false` and the cursor is put back on the same email via
the existing `reselectEmail`.

**Never silently stale.** A cached list is only ever shown with a
refresh in flight, and the header shows `↻` next to the tabs while
`refreshing` is true. If the refresh fails, `↻` clears and the error is
in the status line; the cached list stays (better than an empty screen)
but the failure is visible. The cache is also updated by every
optimistic removal (Layer 3) and by the 5-minute background Inbox fetch,
so the window in which a cached row can differ from the server is the
duration of one 2-round-trip fetch.

**Acting on a cached row is safe.** IMAP UIDs are stable within a
mailbox's UIDVALIDITY: a UID never refers to a different message. An
action on a row that another device already moved fails with a server
NO, which lands in the error path (status + full reload). The only
theoretical exposure is a UIDVALIDITY change between load and action
(mailbox recreated), and neomd already has that exposure for the minutes
between a load and the user's keypress; the cache adds no new window.

**Hard refresh.** `R` keeps today's spinner reload and bypasses the
cache. `u` undo and every error path use it too. Account switch keeps
per-account caches (key includes the account).

**Prefetch.** After the first successful folder load of the session, a
background command on the background connection fetches every tab
folder that is not cached yet, ToScreen first, one after another, and
emits `folderPrefetchedMsg{account, folder, emails}` per folder. The
handler writes the cache only; it never touches the visible list. This
makes the *first* `Tab` of the session instant too. It is lowest
priority: it shares the background connection with the tab counts, and
it never runs on the primary connection.

**Kill switch.** `[ui] instant_folder_switch = true` (default). `false`
disables the cache and prefetch and restores today's spinner-on-switch
behavior; Layers 1–3 stay active.

Off-tab views (Search, Everything, Thread, Sender, Merge, Drafts, Spam)
are not cached; they keep today's behavior.

## Round trips after

| Action | Today | RTTs before redraw (then async on primary) | Perceived |
|---|---|---|---|
| Folder switch, seen before | 3 (+4 queued) | 0 (2 behind the list, primary connection) | instant |
| Folder switch, first time | 3 (+4 queued) | 2 (+1 on bg connection) | ~2 RTT |
| Screen-in `I` on one row | ~10 | 0 (3 behind the list, primary connection) | instant |
| Delete/archive/move N rows | 2N + 7 | 0 (N behind the list, primary connection) | instant |
| Open email | 1–2 | 1–2 | unchanged |
| Search across folders | 2 per folder | 1 per folder | halved |

## Testing

The account this ships to is production email. Every layer is pinned
before it lands, and the whole suite is run before and after each layer.

**T0 — Baseline.** Before any change: `go test ./...`, `go vet ./...`,
`go test ./... -run Hardening`, `make test-integration`. Record the
Layer-0 trace of a `make demo-hp` session doing: switch Inbox→ToScreen→
Inbox, screen-in one row, delete one row, archive one row. This is the
"before" table for the CHANGELOG.

**T1 — Protocol tests without network** (`internal/imap/memserver_test.go`,
new). go-imap v2 ships `imapserver/imapmemserver`, an in-memory IMAP
server. Tests start it on loopback with a self-signed TLS certificate,
seed mailboxes, point `imap.Client` at it (via `TLSCertFile` or the
existing insecure-localhost fallback) and assert on results:

- `FetchHeaders` returns the newest `n` UIDs in newest-first order after
  pipelining, identical to a serial SELECT/SEARCH/FETCH on the same data.
- `FetchHeaders` on a missing mailbox returns the SELECT error and the
  connection stays usable for the next call.
- `FetchUnseenCounts` returns the right unseen count for four mailboxes
  and skips a missing one.
- `MoveMessage` then `FetchHeaders(src)` omits the UID and
  `FetchHeaders(dst)` includes it; three moves in a row all succeed
  without re-SELECT; `destUID` is the COPYUID value.
- `SearchUIDs`, `FetchHeadersByUID`, `searchFolder` unchanged results.

**T2 — UI workflow tests** (`internal/ui/instant_test.go`, new, in the
style of `workflow_hardening_test.go`: drive `Update` with real messages):

- `x`/`A`/`M*`/`I` remove the row immediately, `loading` stays false,
  cursor lands on the next row, marks cleared, returned command is
  non-nil.
- `batchDoneMsg{}` → status "Moved.", undo pushed, `refreshing` true,
  `loading` false.
- `batchDoneMsg{err}` → status error, `loading` true (reload).
- `I` on a single unmarked ToScreen row removes every row with the same
  normalized sender; with marks or in Inbox it removes only targets.
- Auto-screen moves are absent from the list before the first draw;
  `autoScreenDoneMsg{err}` reloads.
- Switch to a cached folder → list filled, `refreshing` true, `loading`
  false; to an uncached one → `loading` true.
- `emailsLoadedMsg` for a non-active folder is cached, not displayed;
  for the active folder is displayed and clears `refreshing`; for a
  different account is cached under that account.
- `R` always goes through the spinner path.
- `folderPrefetchedMsg` fills the cache and does not change `m.emails`.
- `instant_folder_switch = false` → switch uses the spinner path.
- Header renders `↻` iff `refreshing`.
- `bgImapCli()` falls back to the primary client when the background one
  is nil; nil primary stays nil.

**T3 — Live integration tests** (`internal/integration_test.go`, run by
`make test-integration` against the demo account):

- Pipelined `FetchHeaders` returns the same UID set as
  `SearchUIDs` + `FetchHeadersByUID` on a real server.
- MOVE without re-SELECT: append a message, move it, verify source omits
  and destination lists it, move it back.
- `FetchUnseenCounts` matches per-folder `STATUS` issued serially.

**T4 — Hardening suite.** `go test ./... -run Hardening` and
`make test-integration` after every layer that touches `internal/imap`
or the move path (AGENTS.md rule).

**T5 — Manual before/after** on `make demo-hp` with
`NEOMD_IMAP_TRACE=1`: the same script as T0; the after-table goes into
the CHANGELOG entry. Then a day of real use on the production config
with the trace on, checking `moves.log` and `imap-trace.log` against
what was done.

## Documentation

- `AGENTS.md`: new invariants under *IMAP & Runtime Resilience* (pipelined
  SELECT, MOVE keeps selection, background connection never used for user
  actions, trace log) and *Inbox Display* (optimistic removal never trusted
  past an error; cached list only shown with `↻` and a refresh in flight;
  `emailsLoadedMsg` folder/account guard), each with its pinning test.
- `CHANGELOG.md`: one entry per layer with before/after round-trip and
  millisecond tables.
- `docs/content/docs/configuration/_index.md`: `instant_folder_switch`,
  `NEOMD_IMAP_TRACE`.
- README benchmark section: add the round-trips-per-action table; it
  explains provider latency better than the raw SELECT/FETCH numbers.
