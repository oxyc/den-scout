# Follow-ups

Deferred items from the TypeScript → Go port. Everything else in the audit was folded into
the port itself (see the commit history and the intentional-deviation notes in the code).

## #13 — RD/Premiumize positional `fileIdx`

**Deferred, not done.**

Real-Debrid and Premiumize identify a file inside a torrent by its **position** in the
torrent's file list, not by the Torrentio `fileIdx` (which is Torrentio's own index into a
possibly-different ordering). For a single-file movie this is a non-issue; for multi-file
packs the current resolve path finds the right file by episode-matching the *names*
(`pickEpisodeFile`), which is correct but does not exercise a caller-supplied positional
index for RD/PM.

Doing this properly means:
- Fetching each store's own file listing and mapping Torrentio's `fileIdx` → the store's
  positional index (they are not guaranteed to agree on ordering).
- Deciding precedence when both a positional `fileIdx` and an episode selector are present.

Until then, RD/PM resolution relies on name-based episode matching + largest-file fallback,
which covers the common cases. TorBox is unaffected (it addresses files by its own `id`).
