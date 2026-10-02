You are drafting the GitHub release notes for neomd {{VERSION}} (a keyboard-first TUI email client). Write them to the file RELEASE_NOTES.md in the current directory, overwriting it. Do not commit, do not touch any other file, do not run the test suite.

Sources, in this order of authority:
1. CHANGELOG.md — the dated sections newer than the previous release {{PREV}}. Find that date with `git log -1 --format=%ad --date=short {{PREV}}` and read every `# YYYY-MM-DD` section after it (the newest sections are at the top of the file). Each bullet there already explains what changed and why; condense, do not copy.
2. `git log --no-merges --format='%s' {{PREV}}..HEAD` for anything the CHANGELOG missed.
3. The current RELEASE_NOTES.md, only as a template for shape and tone — its content belongs to the previous release and must be replaced.

Structure (exactly this, Markdown):

## What has changed?

<Two or three sentences a user can read in ten seconds: the one theme of this release and what it means when they use neomd. No commit hashes, no function names.>

**Highlights**

- 3 to 6 bullets. Each starts with a bold lead phrase, then one or two sentences. User-visible behavior first; name the key or config option in backticks when there is one. Skip internals unless they change what the user sees or configures.

**Fixes**

- Bullets for bugs a user could have hit, phrased as what went wrong and what is true now. Group small related ones into one bullet. Omit test-only, docs-only and refactor work.

<Optional last line: one sentence on verification or a pointer to a docs page, only if it earns its place.>

Rules: plain English, no marketing adjectives, no exclamation marks, no emoji, no headings other than the one `##` above and the two bold labels. Do not mention this prompt, Claude, or AI. If the range since {{PREV}} contains nothing user-visible, say so in the intro and leave the bullet sections short rather than inventing content. Finish by printing the file's first ten lines so the caller can see it was written.
