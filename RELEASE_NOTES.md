## What has changed?

neomd 0.10 makes everyday email instant on any connection: switching folders, screening, deleting, moving and toggling read/unread now redraw the list immediately and let the IMAP work run behind it, instead of waiting for one to ten sequential round trips per keypress. Opening a large email no longer downloads its attachments first; they are fetched on demand.

**Highlights**

- **Instant folder switch** — a per-folder cache shows the last list at once with a small `↻` while it refreshes; tab folders are prefetched after start. Turn off with `[ui].instant_folder_switch = false`.
- **Instant screening, delete, archive, move** — rows disappear on the keypress; the MOVE runs behind the list. Any error reloads from the server with the error shown, and undo still uses the server's destination UIDs.
- **Instant read/unread toggle** (`n`) — the flag flips locally, the STORE follows, and a refresh landing in between can no longer snap it back.
- **Half the IMAP round trips** — SELECT is pipelined with the first command, tab counts use one STATUS batch instead of four, and MOVE no longer forces a re-SELECT. Housekeeping (tab counts, 5-minute sync, spy scan, prefetch) runs on a second connection so it never queues in front of you.
- **Large mail opens immediately** — for messages ≥ 1 MB only the text parts are fetched; attachments over 256 KB stay on the server, show their size, and download when you press `1`–`9`, forward, reopen a draft with `E`, or act on an invite.
- **`NEOMD_IMAP_TRACE=1`** writes per-operation timings to `~/.cache/neomd/imap-trace.log` for the next time something feels slow.

**Fixes**

- `:screen` / `S` on a tab other than Inbox could move unrelated Inbox mail that shared the same UIDs; both now run on the Inbox tab only, and every MOVE uses the email's own folder.
- The 5-minute background sync screened mail even with empty lists or `auto_screen_on_load = false` (fresh installs could see their whole Inbox moved to ToScreen).
- A failing auto-screen MOVE no longer loops; marks and the `/` filter survive a background refresh; `U` waits for an in-flight move; the `·` reply dot no longer disappears until the next refresh.
- Reply quotes carry the original's date; `ctrl+d` scrolls the list.
- Help overlay lists `:thread`, `:recover`, `:scan-spy-pixels`; the inbox header says `ctrl+u to clear`; the OAuth2 config example includes `auth_type = "oauth2"`.

Every layer is pinned by tests against an in-memory IMAP server and live against Hostpoint; the round-trip table is in the README's Benchmark section.
