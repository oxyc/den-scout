package scout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"
)

// maxStoreBytes caps a debrid API response body — these JSON payloads are small; the limit stops a
// hostile/misbehaving store from OOMing the container (mirrors maxScrapeBytes for indexers).
const maxStoreBytes = 4 << 20

// Debrid stores (ported from src/stores/*). Two ops: CacheCheck (which hashes are cached?) and Resolve
// (infohash → playable https link). Scout resolves server-side; the token never leaves the server.

// ResolveTarget: an infohash + either the exact file, or the series episode to pick from a pack.
type ResolveTarget struct {
	InfoHash string
	FileIdx  *int
	Season   *int
	Episode  *int
}

// Store is a debrid backend. CacheCheck always returns a full map (missing hashes → false); the error
// is non-nil only when the check itself could not be performed (API unreachable/non-200 for every
// batch), which lets the pool distinguish "not cached" from "couldn't tell" and avoid caching an
// empty list built during a store outage.
type Store interface {
	Service() DebridService
	CacheCheck(ctx context.Context, hashes []string) (map[string]bool, error)
	Resolve(ctx context.Context, t ResolveTarget) (string, error)
	// Status reports on a release the store has been ASKED for but could not deliver yet: it is
	// downloading, not dead. Without this the two are the same 404 to the client, which then blacklists
	// a perfectly good release. `ok` is false when this store knows nothing about the target.
	Status(ctx context.Context, t ResolveTarget) (status StoreStatus, ok bool)
}

// StoreStatus — how far along a queued release is. Progress is 0…1; ETASeconds is nil unless the store
// reports a real one (a made-up countdown is worse than none).
type StoreStatus struct {
	Progress   float64
	ETASeconds *int
	// Bytes per second the service reports for this fetch. A percentage that moves slowly and one that
	// has stopped look identical for minutes; the rate says which immediately, and zero says "stalled"
	// while there is still time to pick something else.
	BytesPerSecond *int64
}

// errCheckFailed marks a cache check that could not reach the store at all.
var errCheckFailed = &DeadLinkError{"cache check failed"}

// DeadLinkError — nothing could deliver the file → the route answers 404 so the client falls through.
type DeadLinkError struct{ Reason string }

func (e *DeadLinkError) Error() string { return "dead_link: " + e.Reason }

// StoreUnavailableError — the SERVICE refused us, which says nothing about the release. A rate limit or a
// 5xx answered as 404 told the client "this release is dead" and it fell through every remaining source
// getting the same answer, then sat on a wait for a download nobody had started. Distinct so the route can
// answer 503 and the app can say the debrid is the problem.
type StoreUnavailableError struct {
	Service DebridService
	Reason  string
}

func (e *StoreUnavailableError) Error() string {
	return "store_unavailable: " + string(e.Service) + " " + e.Reason
}

// readStoreError pulls the service's own explanation out of a failed response, as " (detail)" ready to
// append to a message. TorBox answers `{"error":"ACTIVE_LIMIT","detail":"..."}`; anything unparseable
// falls back to a clipped snippet of the raw body, because a truncated reason still beats none.
func readStoreError(resp *http.Response) string {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Error  any    `json:"error"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(raw, &body) == nil {
		switch {
		case body.Detail != "" && body.Error != nil:
			return fmt.Sprintf(" (%v: %s)", body.Error, body.Detail)
		case body.Detail != "":
			return " (" + body.Detail + ")"
		case body.Error != nil:
			return fmt.Sprintf(" (%v)", body.Error)
		}
	}
	return " (" + strings.TrimSpace(string(raw[:min(len(raw), 200)])) + ")"
}

// storeRefusedUs reports whether an HTTP status is the service declining to serve this account right now —
// a throttle or a fault on their side — rather than a verdict about the torrent.
func storeRefusedUs(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

func magnetFor(infoHash string) string { return "magnet:?xt=urn:btih:" + infoHash }

const cacheBatch = 100 // TorBox/Premiumize hashes per checkcached call

// --- TorBox ---

const torboxAPI = "https://api.torbox.app/v1/api"
const resolveCacheTTL = 6 * time.Hour

type torBoxStore struct {
	token  string
	client doer
	cache  Cache
	api    string
}

func (s *torBoxStore) Service() DebridService { return ServiceTorBox }

func (s *torBoxStore) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+s.token)
	req.Header.Set("accept", "application/json")
	return s.client.Do(req)
}

func (s *torBoxStore) CacheCheck(ctx context.Context, hashes []string) (map[string]bool, error) {
	result := make(map[string]bool, len(hashes))
	if len(hashes) == 0 {
		return result, nil
	}
	cached := make([]bool, len(hashes)) // distinct-index writes → concurrency-safe without a lock
	batchOK := make([]bool, (len(hashes)+cacheBatch-1)/cacheBatch)
	g, gctx := errgroup.WithContext(ctx)
	for start := 0; start < len(hashes); start += cacheBatch {
		start := start
		end := start + cacheBatch
		if end > len(hashes) {
			end = len(hashes)
		}
		g.Go(func() error {
			batch := hashes[start:end]
			u := fmt.Sprintf("%s/torrents/checkcached?hash=%s&format=object&list_files=false", s.api, strings.Join(batch, ","))
			resp, err := s.get(gctx, u)
			if err != nil {
				return nil
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			var body struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil {
				return nil
			}
			batchOK[start/cacheBatch] = true
			for i, h := range batch {
				if _, ok := body.Data[h]; ok {
					cached[start+i] = true
				} else if _, ok := body.Data[strings.ToUpper(h)]; ok {
					cached[start+i] = true
				}
			}
			return nil
		})
	}
	_ = g.Wait()
	// Only a batch that came back describes its hashes. One that failed leaves them ABSENT from the map,
	// which the pool reads as "we do not know" — writing false there states that TorBox does not hold
	// them, which is a different and costly claim: it drops a genuinely cached release from a cached-only
	// list, or pays an add at play time for a torrent the account already had. With 500 seeds in batches
	// of 100, one timed-out batch made that claim about 100 releases and reported no error at all.
	for i, h := range hashes {
		if batchOK[i/cacheBatch] {
			result[h] = cached[i]
		}
	}
	return result, batchesFailed(batchOK)
}

// batchesFailed reports errCheckFailed only when every batch failed (none returned usable data). A
// partial failure is not an error — the hashes it could not answer for are simply absent from the map.
func batchesFailed(batchOK []bool) error {
	for _, ok := range batchOK {
		if ok {
			return nil
		}
	}
	return errCheckFailed
}

// The torrent id alone, kept apart from the resolve entry so a queued torrent (no file list) is still
// addressable by `Status`.
func torrentIDKey(token, infoHash string) string {
	return "torbox:torrent:" + keyHash(token) + ":" + infoHash
}

// Remembering that the account has NO torrent for a hash, briefly.
//
// The lookup that answers this fetches the account's ENTIRE torrent list, and only a hit was remembered.
// So the miss — which is every poll of a release that was never queued — refetched the whole list each
// time. A client polls /play every couple of seconds for the length of a download, so a ten-minute wait
// was some three hundred full-account-list fetches, each one growing with the account's history.
//
// Deliberately short: an add can land at any moment, and Status is what tells the viewer their download
// is progressing. This only has to collapse a burst of polls, not remember anything for long. A fresh add
// is unaffected either way, since it writes the id and the id is checked first.
const torrentMissTTL = 15 * time.Second

func torrentMissKey(token, infoHash string) string {
	return "torbox:notorrent:" + keyHash(token) + ":" + infoHash
}

// transportKind describes a transport failure WITHOUT its URL. `*url.Error.Error()` embeds the request
// URL, which for TorBox carries the account token in its query string — so the cause is reported and the
// address is dropped. Enough to tell a timeout from a refused connection; never enough to leak a secret.
func transportKind(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		switch {
		case urlErr.Timeout():
			return "timeout"
		case errors.Is(urlErr.Err, context.Canceled):
			return "cancelled"
		default:
			return urlErr.Err.Error()
		}
	}
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

// refusalReason renders an add failure for the backoff cache, keeping the service's own words where it
// gave any — that string is what the probe route later reports and the log later prints.
func refusalReason(err error) string {
	var unavailable *StoreUnavailableError
	if errors.As(err, &unavailable) {
		return unavailable.Reason
	}
	var dead *DeadLinkError
	if errors.As(err, &dead) {
		return dead.Reason
	}
	return err.Error()
}

// refusalReporter — a store that can say it was recently refused for a hash. Optional, because only a
// store with a cache can remember one; a store that doesn't implement it simply never reports a refusal.
type refusalReporter interface {
	RecentRefusal(infoHash string) (string, bool)
}

// RecentRefusal reports the first store that was turned away for this hash a moment ago, with its reason.
//
// The probe route needs this because it cannot discover a refusal on its own: it deliberately never calls
// the endpoint that refuses. Without it a throttled account reads as "nothing is queued", which the client
// cannot distinguish from a release nobody can deliver — and it condemns a healthy one.
func (p *StorePool) RecentRefusal(infoHash string) (DebridService, string, bool) {
	for _, st := range p.stores {
		reporter, ok := st.(refusalReporter)
		if !ok {
			continue
		}
		if reason, refused := reporter.RecentRefusal(infoHash); refused {
			return st.Service(), reason, true
		}
	}
	return "", "", false
}

func (s *torBoxStore) RecentRefusal(infoHash string) (string, bool) {
	if s.cache == nil {
		return "", false
	}
	return s.cache.Get(refusedKey(s.token, infoHash))
}

// refusedKey marks a hash the service just declined to add for this account, so polls stop re-asking.
func refusedKey(token, infoHash string) string {
	return "torbox:refused:" + keyHash(token) + ":" + infoHash
}

// How long to stop asking after the service refuses. Long enough that a poll loop can't sustain a
// throttle, short enough that a passing 500 doesn't cost the viewer a real wait.
const refusalBackoff = time.Minute

type torboxResolveEntry struct {
	TorrentID int           `json:"torrentId"`
	Files     []TorrentFile `json:"files"`
}

func (s *torBoxStore) Resolve(ctx context.Context, t ResolveTarget) (string, error) {
	// List the pack's files for any series episode (even when a fileIdx is present) so we can name-match
	// the episode — Torrentio's fileIdx and TorBox's file ids/order aren't guaranteed to agree.
	needFiles := t.Season != nil && t.Episode != nil
	// Scope by the debrid token: the cached value is a TorBox torrent_id, which is account-scoped.
	// Every per-install store shares one process-global cache, so an infohash-only key would let one
	// user's cached torrent_id be used with another user's token (→ wrong/other-account content).
	key := "torbox:resolve:" + keyHash(s.token) + ":" + t.InfoHash

	// Fast path: a warm entry from an earlier episode of the same pack. Skip it when episode-select is
	// needed but the cached file list is empty (audit #3 — a transient blip would otherwise mis-serve).
	if s.cache != nil {
		if raw, ok := s.cache.Get(key); ok {
			var e torboxResolveEntry
			if json.Unmarshal([]byte(raw), &e) == nil && (!needFiles || len(e.Files) > 0) {
				fileID, perr := selectFileID(e.Files, t)
				if perr != nil {
					// The cached list already proves this pack is the wrong one. Say so now rather than
					// adding the torrent again below to re-learn the same thing.
					return "", perr
				}
				link, err := s.requestDownload(ctx, e.TorrentID, fileID)
				if err == nil {
					return link, nil
				}
				// A throttled link request on a torrent this account ALREADY holds must not fall through
				// to createtorrent: that turns a read into a write, on an account whose whole problem is
				// that it has written too much.
				var unavailable *StoreUnavailableError
				if errors.As(err, &unavailable) {
					return "", err
				}
			}
		}
	}

	// A client polls /play every few seconds for the whole fetch, and every poll that got this far ran a
	// fresh createtorrent. Once TorBox starts throttling, that loop is what keeps it throttled: the add
	// fails, nothing is cached, and the next poll adds again. Back off for a minute after a refusal so a
	// wait costs one call rather than one per poll.
	if s.cache != nil {
		if reason, ok := s.cache.Get(refusedKey(s.token, t.InfoHash)); ok {
			return "", &StoreUnavailableError{ServiceTorBox, reason + " (backing off)"}
		}
	}
	torrentID, err := s.addMagnet(ctx, t.InfoHash)
	if err != nil {
		// Every refused add backs off, not only a 429. The refusal that caused the incident was a 400 —
		// TorBox's answer for an account at its download limit — which is a `DeadLinkError`, so keying the
		// backoff on the error TYPE left the one case that mattered re-adding on every poll.
		if s.cache != nil {
			s.cache.Put(refusedKey(s.token, t.InfoHash), refusalReason(err), refusalBackoff)
		}
		return "", err
	}
	var files []TorrentFile
	if needFiles {
		files = s.listFiles(ctx, torrentID)
	}
	// audit #3: don't cache an empty file list when we needed one (avoids poisoning the pack for 6h).
	if s.cache != nil && (!needFiles || len(files) > 0) {
		if b, e := json.Marshal(torboxResolveEntry{TorrentID: torrentID, Files: files}); e == nil {
			s.cache.Put(key, string(b), resolveCacheTTL)
		}
	}
	// The torrent id is remembered separately and unconditionally, because `Status` needs it in exactly
	// the case the entry above refuses to write: a just-queued torrent has no file list yet, and that is
	// what makes an episode's first /play look "dead" instead of "downloading".
	if s.cache != nil {
		s.cache.Put(torrentIDKey(s.token, t.InfoHash), strconv.Itoa(torrentID), resolveCacheTTL)
	}
	fileID, err := selectFileID(files, t)
	if err != nil {
		return "", err
	}
	return s.requestDownload(ctx, torrentID, fileID)
}

func (s *torBoxStore) addMagnet(ctx context.Context, infoHash string) (int, error) {
	form := url.Values{"magnet": {magnetFor(infoHash)}, "seed": {"3"}, "allow_zip": {"false"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+"/torrents/createtorrent", strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("authorization", "Bearer "+s.token)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// TorBox explains itself in the body — an account at its active-download limit and a malformed
		// magnet are both a bare 400, and only the text says which. Discarding it left the status code as
		// the entire diagnosis, which is how a hard refusal came to look like a slow download.
		detail := readStoreError(resp)
		if storeRefusedUs(resp.StatusCode) {
			return 0, &StoreUnavailableError{ServiceTorBox,
				fmt.Sprintf("createtorrent http %d%s", resp.StatusCode, detail)}
		}
		return 0, &DeadLinkError{fmt.Sprintf("torbox createtorrent http %d%s", resp.StatusCode, detail)}
	}
	var body struct {
		Data *struct {
			TorrentID *int `json:"torrent_id"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil || body.Data == nil || body.Data.TorrentID == nil {
		return 0, &DeadLinkError{"torbox no torrent_id"}
	}
	return *body.Data.TorrentID, nil
}

// Status — progress for a torrent this account has already been asked to fetch. Resolve records the
// torrent id unconditionally, so a queued release (add succeeded, link not ready) is exactly the case
// this answers.
//
// Reports ok=false unless TorBox positively describes an unfinished download. Anything less — a
// `success:false` body, a `data:null` for a torrent the user deleted, a payload with neither progress nor
// a finished flag — must read as "no wait to promise", or a genuinely dead release would answer
// "downloading, 0%" forever.
func (s *torBoxStore) Status(ctx context.Context, t ResolveTarget) (StoreStatus, bool) {
	torrentID, ok := s.torrentID(ctx, t.InfoHash)
	if !ok {
		return StoreStatus{}, false
	}
	resp, err := s.get(ctx, fmt.Sprintf("%s/torrents/mylist?id=%d&bypass_cache=true", s.api, torrentID))
	if err != nil {
		return StoreStatus{}, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return StoreStatus{}, false
	}
	var body struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil {
		return StoreStatus{}, false
	}
	// TorBox answers 200 with success:false / data:null for an id it no longer holds, and unmarshalling
	// null into a struct succeeds silently — so both have to be rejected explicitly.
	if body.Success != nil && !*body.Success {
		return StoreStatus{}, false
	}
	type entryState struct {
		Progress         *float64 `json:"progress"`
		DownloadFinished *bool    `json:"download_finished"`
		ETA              *int     `json:"eta"`
		DownloadSpeed    *int64   `json:"download_speed"`
	}
	var st entryState
	if len(body.Data) == 0 || json.Unmarshal(body.Data, &st) != nil {
		var arr []entryState
		if json.Unmarshal(body.Data, &arr) != nil || len(arr) == 0 {
			return StoreStatus{}, false
		}
		st = arr[0]
	}
	// Finished but Resolve still failed → not a wait we can promise anything about; reads as dead.
	if st.DownloadFinished != nil && *st.DownloadFinished {
		return StoreStatus{}, false
	}
	// Nothing said about the download at all (an empty object, a deleted torrent) — not a wait either.
	if st.Progress == nil && st.DownloadFinished == nil {
		return StoreStatus{}, false
	}
	out := StoreStatus{}
	if st.Progress != nil {
		out.Progress = *st.Progress
	}
	if st.ETA != nil && *st.ETA > 0 {
		out.ETASeconds = st.ETA
	}
	// Zero is meaningful here — it is the stall — so only a missing or negative figure is dropped.
	if st.DownloadSpeed != nil && *st.DownloadSpeed >= 0 {
		out.BytesPerSecond = st.DownloadSpeed
	}
	return out, true
}

// torrentID finds the account's torrent id for an infohash: from the cache Resolve wrote, and failing
// that by asking TorBox for the account's list and matching on hash.
//
// The fallback exists because the cached id is the ONLY thing that made a queued release describable, and
// it is lost by any of a redeploy, a pruned cache, or an add that recorded nothing. Without it `Status`
// answers "nothing here" while the file downloads perfectly well — which a client can only read as a dead
// link. Rediscovering the id costs one list call, and only on the path that was previously a dead end.
func (s *torBoxStore) torrentID(ctx context.Context, infoHash string) (int, bool) {
	if s.cache != nil {
		if raw, ok := s.cache.Get(torrentIDKey(s.token, infoHash)); ok {
			if id, err := strconv.Atoi(raw); err == nil {
				return id, true
			}
		}
	}
	if s.cache != nil {
		if _, missed := s.cache.Get(torrentMissKey(s.token, infoHash)); missed {
			return 0, false
		}
	}
	id, ok := s.findTorrentByHash(ctx, infoHash)
	if !ok {
		// Remember the miss too, or every poll re-fetches the whole account list to learn the same thing.
		if s.cache != nil {
			s.cache.Put(torrentMissKey(s.token, infoHash), "1", torrentMissTTL)
		}
		return 0, false
	}
	// Remember it, so the next poll of this wait is a single-id lookup again rather than another list.
	if s.cache != nil {
		s.cache.Put(torrentIDKey(s.token, infoHash), strconv.Itoa(id), resolveCacheTTL)
	}
	return id, true
}

// findTorrentByHash scans the account's torrent list for an infohash. TorBox reports hashes lower-case;
// indexers are inconsistent about it, so both sides are folded before comparing.
func (s *torBoxStore) findTorrentByHash(ctx context.Context, infoHash string) (int, bool) {
	if s.client == nil {
		return 0, false
	}
	resp, err := s.get(ctx, s.api+"/torrents/mylist?bypass_cache=true")
	if err != nil {
		return 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	var body struct {
		Data []struct {
			ID   int    `json:"id"`
			Hash string `json:"hash"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil {
		return 0, false
	}
	want := strings.ToLower(infoHash)
	for _, e := range body.Data {
		if strings.ToLower(e.Hash) == want {
			return e.ID, true
		}
	}
	return 0, false
}

func (s *torBoxStore) listFiles(ctx context.Context, torrentID int) []TorrentFile {
	resp, err := s.get(ctx, fmt.Sprintf("%s/torrents/mylist?id=%d&bypass_cache=true", s.api, torrentID))
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var body struct {
		Data json.RawMessage `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil {
		return nil
	}
	type tbFile struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		Size      *int   `json:"size"`
	}
	type entry struct {
		Files []tbFile `json:"files"`
	}
	var e entry
	if json.Unmarshal(body.Data, &e) != nil {
		var arr []entry
		if json.Unmarshal(body.Data, &arr) != nil || len(arr) == 0 {
			return nil
		}
		e = arr[0]
	}
	out := make([]TorrentFile, 0, len(e.Files))
	for _, f := range e.Files {
		name := f.Name
		if name == "" {
			name = f.ShortName
		}
		out = append(out, TorrentFile{Index: f.ID, Name: name, SizeBytes: f.Size})
	}
	return out
}

func (s *torBoxStore) requestDownload(ctx context.Context, torrentID int, fileID *int) (string, error) {
	q := url.Values{"token": {s.token}, "torrent_id": {fmt.Sprintf("%d", torrentID)}}
	if fileID != nil {
		q.Set("file_id", fmt.Sprintf("%d", *fileID))
	}
	resp, err := s.get(ctx, s.api+"/torrents/requestdl?"+q.Encode())
	if err != nil {
		// Never the raw error. TorBox wants the token as a QUERY PARAMETER, and a transport failure yields
		// a *url.Error whose message is `Get "<the whole URL>": …` — Go redacts userinfo passwords, not
		// query strings. Both the resolve log and the /play error line print %v of whatever comes back
		// here, so one timeout would have written the live debrid token into a rotated, shipped container
		// log. `docs/SEALED-CONFIG.md` lists "the token is never logged" as an acceptance criterion, and
		// the whole sealing design exists because the URL *is* the credential.
		return "", &DeadLinkError{"torbox requestdl transport: " + transportKind(err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if storeRefusedUs(resp.StatusCode) {
		return "", &StoreUnavailableError{ServiceTorBox, fmt.Sprintf("requestdl http %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return "", &DeadLinkError{fmt.Sprintf("torbox requestdl http %d", resp.StatusCode)}
	}
	var body struct {
		Success bool   `json:"success"`
		Data    string `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil || !body.Success || body.Data == "" {
		return "", &DeadLinkError{"torbox no link"}
	}
	return body.Data, nil
}

// selectFileID picks TorBox's own file id for the requested file. For a series episode it name-matches
// the pack first (most reliable); a fileIdx is a POSITION in the torrent's file list, so it's mapped to
// TorBox's file id via the loaded list. Without a file list (single-file fast path / list failure) the
// raw fileIdx is passed through best-effort — TorBox ignores file_id for a single-file torrent.
// A pack that demonstrably holds other episodes and not this one is refused rather than substituted for:
// the indexer's fileIdx is a position in a list we can now see does not contain the episode, so trusting
// it would only pick the wrong file by a different route.
func selectFileID(files []TorrentFile, t ResolveTarget) (*int, error) {
	if t.Season != nil && t.Episode != nil {
		id, err := pickEpisodeFile(files, *t.Season, *t.Episode)
		if err != nil {
			return nil, err
		}
		if id != nil {
			return id, nil
		}
	}
	if t.FileIdx != nil {
		if *t.FileIdx >= 0 && *t.FileIdx < len(files) {
			return &files[*t.FileIdx].Index, nil
		}
		return t.FileIdx, nil
	}
	return nil, nil
}

// --- Real-Debrid ---

const realDebridAPI = "https://api.real-debrid.com/rest/1.0"

type realDebridStore struct {
	token  string
	client doer
	api    string
}

func (s *realDebridStore) Service() DebridService { return ServiceRealDebrid }

// CacheCheck: RD has no usable cache API → all-false, and nil error (this is authoritative "nothing
// known cached via RD", not a failure — RD contributes no cache truth by design).
func (s *realDebridStore) CacheCheck(_ context.Context, hashes []string) (map[string]bool, error) {
	result := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		result[h] = false
	}
	return result, nil
}

func (s *realDebridStore) post(ctx context.Context, path string, form url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+s.token)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	return s.client.Do(req)
}

type rdInfo struct {
	Files []struct {
		ID    int    `json:"id"`
		Path  string `json:"path"`
		Bytes int    `json:"bytes"`
	} `json:"files"`
	Links []string `json:"links"`
}

// Real-Debrid exposes no queue state we can poll per infohash here, so it never reports a wait — a
// failure stays a failure rather than becoming a promise we can't keep.
func (s *realDebridStore) Status(context.Context, ResolveTarget) (StoreStatus, bool) {
	return StoreStatus{}, false
}

func (s *realDebridStore) Resolve(ctx context.Context, t ResolveTarget) (string, error) {
	addResp, err := s.post(ctx, "/torrents/addMagnet", url.Values{"magnet": {magnetFor(t.InfoHash)}})
	if err != nil {
		return "", err
	}
	var added struct {
		ID string `json:"id"`
	}
	dec := json.NewDecoder(io.LimitReader(addResp.Body, maxStoreBytes))
	_ = dec.Decode(&added)
	_ = addResp.Body.Close()
	// A refusal is a fact about the account, not the release — the same distinction TorBox already draws.
	// Every non-2xx here used to become a dead link, so on a Real-Debrid install a 429 or a 503 reached
	// the app as "this release does not exist", and the player walked the whole candidate list collecting
	// the identical non-answer and condemning healthy releases on the way.
	if storeRefusedUs(addResp.StatusCode) {
		return "", &StoreUnavailableError{ServiceRealDebrid,
			fmt.Sprintf("addmagnet http %d", addResp.StatusCode)}
	}
	if addResp.StatusCode < 200 || addResp.StatusCode >= 300 || added.ID == "" {
		return "", &DeadLinkError{"realdebrid no torrent id"}
	}

	info, err := s.info(ctx, added.ID)
	if err != nil {
		return "", err
	}
	files := make([]TorrentFile, len(info.Files))
	for i, f := range info.Files {
		size := f.Bytes
		files[i] = TorrentFile{Index: f.ID, Name: f.Path, SizeBytes: &size}
	}
	fileID, err := s.pickFileID(files, t)
	if err != nil {
		return "", err
	}
	if fileID == nil {
		return "", &DeadLinkError{"realdebrid no file"}
	}
	// RD rejects anti-piracy-matched filenames — fail fast so the pool falls through to another store.
	for _, f := range files {
		if f.Index == *fileID && realDebridBlocked(f.Name) {
			return "", &DeadLinkError{"realdebrid blocked filename"}
		}
	}

	sel, err := s.post(ctx, "/torrents/selectFiles/"+added.ID, url.Values{"files": {fmt.Sprintf("%d", *fileID)}})
	if err != nil {
		return "", err
	}
	_ = sel.Body.Close()
	if sel.StatusCode < 200 || sel.StatusCode >= 300 {
		return "", &DeadLinkError{fmt.Sprintf("realdebrid selectFiles http %d", sel.StatusCode)}
	}
	ready, err := s.info(ctx, added.ID)
	if err != nil {
		return "", err
	}
	if len(ready.Links) == 0 {
		return "", &DeadLinkError{"realdebrid not ready"}
	}
	return s.unrestrict(ctx, ready.Links[0])
}

func (s *realDebridStore) info(ctx context.Context, id string) (*rdInfo, error) {
	// PathEscape + a checked error: id comes from RD's addMagnet response (untrusted); a stray byte
	// would otherwise make NewRequest return a nil *Request and the next line panic.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.api+"/torrents/info/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, &DeadLinkError{"realdebrid bad torrent id"}
	}
	req.Header.Set("authorization", "Bearer "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, &DeadLinkError{fmt.Sprintf("realdebrid info http %d", resp.StatusCode)}
	}
	var info rdInfo
	if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&info) != nil {
		return nil, &DeadLinkError{"realdebrid bad info"}
	}
	return &info, nil
}

func (s *realDebridStore) unrestrict(ctx context.Context, link string) (string, error) {
	resp, err := s.post(ctx, "/unrestrict/link", url.Values{"link": {link}})
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &DeadLinkError{fmt.Sprintf("realdebrid unrestrict http %d", resp.StatusCode)}
	}
	var body struct {
		Download string `json:"download"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil || body.Download == "" {
		return "", &DeadLinkError{"realdebrid no download"}
	}
	return body.Download, nil
}

func (s *realDebridStore) pickFileID(files []TorrentFile, t ResolveTarget) (*int, error) {
	if len(files) == 0 {
		return nil, nil
	}
	// Series episode: name-match against the pack first — Torrentio's fileIdx and RD's file order aren't
	// guaranteed to agree, so a positional index can pick the wrong episode. A pack that names other
	// episodes and not this one is refused, not approximated by the fileIdx or the largest file.
	if t.Season != nil && t.Episode != nil {
		id, err := pickEpisodeFile(files, *t.Season, *t.Episode)
		if err != nil {
			return nil, err
		}
		if id != nil {
			return id, nil
		}
	}
	if t.FileIdx != nil {
		if *t.FileIdx >= 0 && *t.FileIdx < len(files) {
			return &files[*t.FileIdx].Index, nil
		}
		return &files[0].Index, nil
	}
	idx := largest(files).Index
	return &idx, nil
}

// --- Premiumize ---

const premiumizeAPI = "https://www.premiumize.me/api"

type premiumizeStore struct {
	token  string
	client doer
	api    string
}

func (s *premiumizeStore) Service() DebridService { return ServicePremiumize }

func (s *premiumizeStore) CacheCheck(ctx context.Context, hashes []string) (map[string]bool, error) {
	result := make(map[string]bool, len(hashes))
	if len(hashes) == 0 {
		return result, nil
	}
	cached := make([]bool, len(hashes))
	batchOK := make([]bool, (len(hashes)+cacheBatch-1)/cacheBatch)
	g, gctx := errgroup.WithContext(ctx)
	for start := 0; start < len(hashes); start += cacheBatch {
		start := start
		end := start + cacheBatch
		if end > len(hashes) {
			end = len(hashes)
		}
		g.Go(func() error {
			batch := hashes[start:end]
			q := url.Values{"apikey": {s.token}}
			for _, h := range batch {
				q.Add("items[]", h)
			}
			req, err := http.NewRequestWithContext(gctx, http.MethodGet, s.api+"/cache/check?"+q.Encode(), nil)
			if err != nil {
				return nil
			}
			resp, err := s.client.Do(req)
			if err != nil {
				return nil
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			var body struct {
				Status   string `json:"status"`
				Response []bool `json:"response"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil || body.Status != "success" {
				return nil
			}
			batchOK[start/cacheBatch] = true
			for i := range batch {
				if i < len(body.Response) && body.Response[i] {
					cached[start+i] = true
				}
			}
			return nil
		})
	}
	_ = g.Wait()
	// As with TorBox: a failed batch leaves its hashes absent rather than claiming they are not cached.
	for i, h := range hashes {
		if batchOK[i/cacheBatch] {
			result[h] = cached[i]
		}
	}
	return result, batchesFailed(batchOK)
}

// Premiumize likewise: cached or not, with no in-between to report.
func (s *premiumizeStore) Status(context.Context, ResolveTarget) (StoreStatus, bool) {
	return StoreStatus{}, false
}

func (s *premiumizeStore) Resolve(ctx context.Context, t ResolveTarget) (string, error) {
	form := url.Values{"apikey": {s.token}, "src": {magnetFor(t.InfoHash)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+"/transfer/directdl", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	// Same rule as the other two stores: being turned away says nothing about the release.
	if storeRefusedUs(resp.StatusCode) {
		return "", &StoreUnavailableError{ServicePremiumize,
			fmt.Sprintf("directdl http %d", resp.StatusCode)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &DeadLinkError{fmt.Sprintf("premiumize directdl http %d", resp.StatusCode)}
	}
	var body struct {
		Status  string `json:"status"`
		Content []struct {
			Path string `json:"path"`
			Link string `json:"link"`
			Size *int   `json:"size"`
		} `json:"content"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil || body.Status != "success" || len(body.Content) == 0 {
		return "", &DeadLinkError{"premiumize no content"}
	}
	files := make([]TorrentFile, len(body.Content))
	for i, c := range body.Content {
		files[i] = TorrentFile{Index: i, Name: c.Path, SizeBytes: c.Size}
	}
	idx, err := s.pickIndex(files, t)
	if err != nil {
		return "", err
	}
	if idx == nil || *idx < 0 || *idx >= len(body.Content) || body.Content[*idx].Link == "" {
		return "", &DeadLinkError{"premiumize no link"}
	}
	return body.Content[*idx].Link, nil
}

func (s *premiumizeStore) pickIndex(files []TorrentFile, t ResolveTarget) (*int, error) {
	if len(files) == 0 {
		return nil, nil
	}
	// Series episode: name-match against the pack first — Torrentio's fileIdx and Premiumize's content
	// order aren't guaranteed to agree, so a positional index can pick the wrong episode. A pack that
	// names other episodes and not this one is refused, not approximated by the fileIdx or largest file.
	if t.Season != nil && t.Episode != nil {
		id, err := pickEpisodeFile(files, *t.Season, *t.Episode)
		if err != nil {
			return nil, err
		}
		if id != nil {
			return id, nil
		}
	}
	if t.FileIdx != nil && *t.FileIdx >= 0 && *t.FileIdx < len(files) {
		return t.FileIdx, nil
	}
	idx := largest(files).Index
	return &idx, nil
}

// --- pool ---

// StorePool builds one store per account in service-priority order (TorBox first).
type StorePool struct{ stores []Store }

func buildStores(config *Config, client doer, cache Cache) []Store {
	byService := make(map[DebridService]string)
	for _, d := range config.Debrid {
		byService[d.Service] = d.Token
	}
	var stores []Store
	for _, svc := range debridServices {
		token, ok := byService[svc]
		if !ok {
			continue
		}
		switch svc {
		case ServiceTorBox:
			stores = append(stores, &torBoxStore{token: token, client: client, cache: cache, api: torboxAPI})
		case ServiceRealDebrid:
			stores = append(stores, &realDebridStore{token: token, client: client, api: realDebridAPI})
		case ServicePremiumize:
			stores = append(stores, &premiumizeStore{token: token, client: client, api: premiumizeAPI})
		}
	}
	return stores
}

// CacheTruth is what the pool actually learned about a batch of hashes.
//
// Not a bare map of yes/no, because "no" and "we could not find out" have opposite costs and were being
// reported identically. A false "not cached" drops a playable release from a cached-only list, or pays a
// debrid ADD at play time for a torrent the account already held — against a sixty-an-hour ceiling. And
// WHICH service holds a release matters as much as whether one does: the union said "cached" for a
// release only the second account had, and the probe path then resolved it against the first, adding it.
type CacheTruth struct {
	holders map[string][]DebridService
	known   map[string]bool
}

// Cached — at least one store confirmed it holds this.
func (t CacheTruth) Cached(hash string) bool { return len(t.holders[hash]) > 0 }

// Known — at least one cache-truth store gave an answer for this hash, either way. False means unknown,
// which is NOT the same as not cached.
func (t CacheTruth) Known(hash string) bool { return t.known[hash] }

// HeldBy — the services that confirmed they hold it, in configured priority order. Resolving against
// anything outside this list may add the torrent rather than fetch it.
func (t CacheTruth) HeldBy(hash string) []DebridService { return t.holders[hash] }

// CacheCheck asks every store at once. truthOK reports whether at least one cache-truth store
// (TorBox/Premiumize) answered at all; when false the handler skips the cached-only filter (rather than
// dropping everything) and declines to cache the degraded list.
func (p *StorePool) CacheCheck(ctx context.Context, hashes []string) (CacheTruth, bool) {
	truth := CacheTruth{
		holders: make(map[string][]DebridService, len(hashes)),
		known:   make(map[string]bool, len(hashes)),
	}
	if len(hashes) == 0 {
		return truth, true
	}
	// Independent per store; run concurrently. A store error can't 500 the request (audit #5) — it only
	// withholds that store's truth.
	maps := make([]map[string]bool, len(p.stores))
	var g errgroup.Group
	for i, st := range p.stores {
		i, st := i, st
		g.Go(func() error {
			// An error means the store learned NOTHING — a partial failure returns no error and simply
			// omits the hashes it could not check, so absence carries that case on its own.
			if m, err := st.CacheCheck(ctx, hashes); err == nil {
				maps[i] = m
			}
			return nil
		})
	}
	_ = g.Wait()
	// Store order, so a hash several services hold lists them in configured priority.
	truthOK := false
	for i, m := range maps {
		cacheTruth := isCacheTruthService(p.stores[i].Service())
		for hash, cached := range m {
			// Presence in the map is the store's claim to have answered for that hash.
			if cacheTruth {
				truth.known[hash] = true
				truthOK = true
			}
			if cached {
				truth.holders[hash] = append(truth.holders[hash], p.stores[i].Service())
			}
		}
	}
	return truth, truthOK
}

func isCacheTruthService(svc DebridService) bool {
	return svc == ServiceTorBox || svc == ServicePremiumize
}

func (p *StorePool) Resolve(ctx context.Context, t ResolveTarget) (string, error) {
	return p.ResolvePreferring(ctx, t, nil)
}

// ResolvePreferring resolves, trying the services in `preferred` before the rest.
//
// The point is not speed but WHAT ARRIVES: a release one service already holds plays now, while the same
// release on another has to be downloaded first — minutes to hours. CacheCheck already learns which
// services hold it and the union threw that away, so the fixed store order could send a viewer to
// download a file that was sitting cached on their other account.
// Every store's failure is logged. The generic "no store could resolve" that replaced them said only that
// something went wrong, which is indistinguishable from a dead release — and an add that never reached the
// debrid (an expired token, a rejected magnet, an exhausted deadline) looks from the outside exactly like a
// release nobody is seeding: the client waits on a download the service was never asked to start.
func (p *StorePool) ResolvePreferring(ctx context.Context, t ResolveTarget,
	preferred []DebridService) (string, error) {
	var refused error
	for _, st := range p.ordered(preferred) {
		link, err := st.Resolve(ctx, t)
		if err == nil {
			return link, nil
		}
		log.Printf("scout: %s could not resolve %s: %v", st.Service(), shortHash(t.InfoHash), err)
		// A service refusing US outranks a dead link as an explanation: if even one store was throttled
		// or faulting, "this release is dead" is not a conclusion the evidence supports.
		var unavailable *StoreUnavailableError
		if errors.As(err, &unavailable) && refused == nil {
			refused = err
		}
	}
	if refused != nil {
		return "", refused
	}
	return "", &DeadLinkError{"no store could resolve"}
}

// shortHash trims an infohash for logs — enough to correlate with the client's line, not the whole thing.
func shortHash(h string) string {
	if len(h) <= 12 {
		return h
	}
	return h[:12]
}

// ordered puts the preferred services first, keeping the configured order within each group so the
// result stays deterministic — the same request must not resolve differently run to run.
func (p *StorePool) ordered(preferred []DebridService) []Store {
	if len(preferred) == 0 {
		return p.stores
	}
	wanted := make(map[DebridService]bool, len(preferred))
	for _, svc := range preferred {
		wanted[svc] = true
	}
	out := make([]Store, 0, len(p.stores))
	for _, st := range p.stores {
		if wanted[st.Service()] {
			out = append(out, st)
		}
	}
	for _, st := range p.stores {
		if !wanted[st.Service()] {
			out = append(out, st)
		}
	}
	return out
}

// ResolveCachedOnly resolves against ONLY the services that are known to hold the release, and fails
// rather than falling through to one that would have to fetch it.
//
// This is the read-only sibling of Resolve, for callers that must never cause an add. Resolve tries every
// configured store in priority order, so with two accounts a release cached on the SECOND one still
// reaches the first — which adds it. That is invisible on a single-account install and burns the whole
// hourly quota on a two-account one, from a background path nobody asked for.
func (p *StorePool) ResolveCachedOnly(ctx context.Context, t ResolveTarget,
	holders []DebridService) (string, error) {
	if len(holders) == 0 {
		return "", &DeadLinkError{"no store holds this release"}
	}
	held := make(map[DebridService]bool, len(holders))
	for _, svc := range holders {
		held[svc] = true
	}
	var refused error
	for _, st := range p.stores {
		if !held[st.Service()] {
			continue
		}
		link, err := st.Resolve(ctx, t)
		if err == nil {
			return link, nil
		}
		var unavailable *StoreUnavailableError
		if errors.As(err, &unavailable) && refused == nil {
			refused = err
		}
	}
	if refused != nil {
		return "", refused
	}
	return "", &DeadLinkError{"no holding store could resolve"}
}

// Status — the first store that can say a queued release is still downloading. Only meaningful right
// after a failed Resolve; ok=false means nobody is fetching it, i.e. genuinely dead.
func (p *StorePool) Status(ctx context.Context, t ResolveTarget) (StoreStatus, bool) {
	for _, st := range p.stores {
		if status, ok := st.Status(ctx, t); ok {
			return status, true
		}
	}
	return StoreStatus{}, false
}

// hasCacheTruth reports whether any configured store has a real cache API (TorBox/Premiumize). When
// false (RD-only), the handler skips the cached-only filter so the list isn't always empty (audit #4).
func hasCacheTruth(config *Config) bool {
	for _, d := range config.Debrid {
		if isCacheTruthService(d.Service) {
			return true
		}
	}
	return false
}
