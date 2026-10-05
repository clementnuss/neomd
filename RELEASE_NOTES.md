## What has changed?

neomd 0.10.1 fixes what the folder list shows in large mailboxes. The list window is now the newest mail by arrival date, so mail you approve, restore or undo no longer buries this week's messages, and you can page further back from the bottom of the list instead of raising `inbox_count` in the config.

**Highlights**

- **Load more at the bottom of the list** — `j`, `d` or `ctrl+d` on the last row fetches the next `inbox_count` older emails, keeps the cursor on its row and reports "Loaded N more · M emails" or "All M emails loaded". While a folder holds more than is loaded, the bottom hint bar starts with "↓ more below (j/d at last row)". The extended list survives `R`, ↻ and tab switches; Search, Everything and Thread views never page, and `inbox_count = 0` is untouched.
- **`ctrl+u` pages up; `esc` clears marks** — `ctrl+u` now pairs with `ctrl+d` as a vim-style half-page alias for `u`. "Clear all marks" moved to `esc`, as the first step of its back-one-level cascade (marks → temporary view → filter), and the header hint reads `[N marked · esc to clear]`.
- **`make status` on the headless server says whether screening works** — it prints the last log lines and either `OK: last screening cycle succeeded` or `ERROR: last screening cycle FAILED` with the error and the time it started failing, exiting 1 in that case, when the daemon is not running, or when the log file is missing. `make sync-headless` runs it after every deploy, so a silent outage like a broken DNS resolver no longer goes unnoticed for weeks.

**Fixes**

- Folders with more than `inbox_count` messages showed the highest UIDs, not the newest mail (#34). A MOVE gives a message a fresh, highest UID in its destination, so every mail approved on ToScreen with `I`, restored with `:reset-toscreen` or undone with `U` landed above real recent mail and pushed it out of the window, where no sort could bring it back. The folder window, `:everything` and the per-folder search cap now keep the most recently received messages by arrival date. Folders within `inbox_count`, and `inbox_count = 0`, take the old path unchanged, so a small Inbox sees no new traffic.

Each change is pinned by tests against the in-memory IMAP server; the window fix was verified against a live Hostpoint mailbox that returned April mail before and October mail after.
