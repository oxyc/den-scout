package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// How long /play waits on the check of a link it just minted. One ranged GET for a single byte; a CDN
// that cannot answer that in three seconds is slow, not proof of anything, and the link goes out anyway.
const linkCheckTimeout = 3 * time.Second

// How far the served size may drift from the size the debrid listed before the link is called wrong.
// Real files match to the byte; the slack is for a service that rounds, not for a different file.
const linkSizeTolerance = 0.02

type linkVerdict int

const (
	// The check could not reach a verdict — a timeout, a transport error, a throttled CDN. Says nothing
	// about the link, so the link is served.
	linkUnverified linkVerdict = iota
	// The link answered like a file: 200 or 206, not a web page, the right size where that is known.
	linkPlayable
	// The link answered, and the answer was not the file.
	linkBroken
)

// checkLink asks a freshly minted link for its first byte and judges the answer.
//
// A debrid hands out links it cannot serve more often than it should: an error page at a CDN URL, a JSON
// error body with a 200, a link to a different file in the pack. The player finds out only after it has
// buffered, shown a spinner, and failed — and the client then cannot tell a bad link from a bad release.
// One ranged byte finds out before the 302 goes out, and a link that fails gets the same 404 dead_link a
// release that could not be resolved gets.
//
// It must never condemn a link for its OWN failure. A timeout or a refused connection is the check not
// getting an answer, and treating that as a dead link would turn a slow CDN into a blacklisted release —
// the exact confusion StoreUnavailableError exists to keep out of this route. The same goes for 429 and
// 5xx: the CDN declining to answer right now, not saying the link is wrong.
func checkLink(ctx context.Context, client *http.Client, link string, wantSize int64) (linkVerdict, string) {
	ctx, cancel := context.WithTimeout(ctx, linkCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return linkBroken, "not a usable URL"
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		// transportKind, never the error itself: its message carries the URL, and a playback link is a
		// credential.
		return linkUnverified, transportKind(err)
	}
	// Bounded, like the probe's drain: a server that ignores the Range answers 200 with the whole file.
	defer func() { _, _ = io.CopyN(io.Discard, resp.Body, 1<<10); _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusPartialContent || resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return linkUnverified, fmt.Sprintf("http %d", resp.StatusCode)
	default:
		return linkBroken, fmt.Sprintf("http %d", resp.StatusCode)
	}
	// An error page served with a success status. No video is served as either of these.
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("content-type")); mt == "text/html" || mt == "application/json" {
		return linkBroken, "served " + mt
	}
	if wantSize > 0 {
		if total, ok := contentRangeTotal(resp.Header.Get("content-range")); ok &&
			math.Abs(float64(total-wantSize)) > linkSizeTolerance*float64(wantSize) {
			return linkBroken, fmt.Sprintf("serves %d bytes, the debrid listed %d", total, wantSize)
		}
	}
	return linkPlayable, ""
}

// contentRangeTotal reads the complete length out of `bytes 0-0/12345`. A `*` total is unknown.
func contentRangeTotal(v string) (int64, bool) {
	_, total, found := strings.Cut(v, "/")
	if !found {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64)
	return n, err == nil && n > 0
}

// verifyLink runs checkLink for /play with whatever the stores already know about the file's size.
//
// No probe client wired means no check, the same opt-in the probe fan-out uses: a caller that has not
// provided one (a test, an embedder) gets the old behaviour exactly.
func (h *handler) verifyLink(ctx context.Context, pool *StorePool, rt ResolveTarget, link string) linkVerdict {
	if h.deps.ProbeClient == nil {
		return linkUnverified
	}
	size, _ := pool.KnownFileSize(rt)
	verdict, reason := checkLink(ctx, h.deps.ProbeClient, link, size)
	switch verdict {
	case linkBroken:
		logLimited("play-link-rejected", "play %s: the minted link failed its check (%s)",
			shortHash(rt.InfoHash), reason)
	case linkUnverified:
		logLimited("play-link-unverified", "play %s: could not check the minted link (%s), serving it anyway",
			shortHash(rt.InfoHash), reason)
	}
	return verdict
}

// fileSizer — a store that already knows, from what it has cached, how large the target file is.
// Optional and cache-only: a size is only worth having if finding it costs nothing.
type fileSizer interface {
	KnownFileSize(t ResolveTarget) (int64, bool)
}

// KnownFileSize is the first size any store knows for this file. Stores that hold the same torrent
// describe the same file, so which one answers does not matter.
func (p *StorePool) KnownFileSize(t ResolveTarget) (int64, bool) {
	for _, st := range p.stores {
		if fs, ok := st.(fileSizer); ok {
			if n, ok := fs.KnownFileSize(t); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// TorBox's size comes out of the resolve entry, which carries the pack's file list whenever an episode
// had to be picked from one. A movie resolves without listing files, so its size is simply not known.
func (s *torBoxStore) KnownFileSize(t ResolveTarget) (int64, bool) {
	if s.cache == nil {
		return 0, false
	}
	raw, ok := s.cache.Get(resolveKey(s.token, t.InfoHash))
	if !ok {
		return 0, false
	}
	var e torboxResolveEntry
	if json.Unmarshal([]byte(raw), &e) != nil || len(e.Files) == 0 {
		return 0, false
	}
	id, err := selectFileID(e.Files, t)
	if err != nil || id == nil {
		return 0, false
	}
	return sizeOf(e.Files, *id)
}

// rdSizeKey — the byte size RD listed for the file a remembered torrent serves. Same scope as the id.
func rdSizeKey(token, infoHash string, t ResolveTarget) string {
	return rdTorrentKey(token, infoHash, t) + ":bytes"
}

// rememberFileSize keeps the size RD's info reported, which no later call would otherwise see: the Store
// interface returns a link and nothing else.
func (s *realDebridStore) rememberFileSize(t ResolveTarget, files []TorrentFile, fileID int) {
	if s.cache == nil {
		return
	}
	if n, ok := sizeOf(files, fileID); ok {
		s.cache.Put(rdSizeKey(s.token, t.InfoHash, t), strconv.FormatInt(n, 10), resolveCacheTTL)
	}
}

func (s *realDebridStore) KnownFileSize(t ResolveTarget) (int64, bool) {
	if s.cache == nil {
		return 0, false
	}
	raw, ok := s.cache.Get(rdSizeKey(s.token, t.InfoHash, t))
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	return n, err == nil && n > 0
}

func sizeOf(files []TorrentFile, id int) (int64, bool) {
	for _, f := range files {
		if f.Index == id && f.SizeBytes != nil && *f.SizeBytes > 0 {
			return int64(*f.SizeBytes), true
		}
	}
	return 0, false
}
