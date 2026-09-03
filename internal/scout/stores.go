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
	// NoAdd forbids queueing the torrent: resolve it from what the account already has, or fail.
	//
	// A background caller cannot promise this by choosing carefully, because whether a Resolve adds is
	// decided deep inside each store. The probe path DID choose carefully — it only resolves releases a
	// cache check reported as held — and still POSTed createtorrent, because TorBox's checkcached reports
	// what TORBOX has, not what this ACCOUNT has. "Held" was never the same as "already added". Only the
	// store can enforce this, so the caller states the requirement and the store obeys it.
	NoAdd bool
}

// errWouldAdd — a NoAdd target could only be resolved by queueing the torrent, so it was not resolved.
var errWouldAdd = &DeadLinkError{"not held by this account (add not permitted)"}

// errTorrentGone — the service answered, and said it has no such torrent. The ONLY evidence that a
// remembered torrent id is stale.
//
// It has to be positive evidence, because the alternative was measured: treating any failure of the
// held path as "the torrent is gone" meant a cancelled poll erased a perfectly good id and re-bought the
// torrent — ten polls of one already-downloaded release cost ten adds against a sixty-an-hour ceiling.
// A cancellation, a throttle, a timeout, and a pack that lacks the requested episode say nothing at all
// about whether the account still holds the file.
var errTorrentGone = &DeadLinkError{"torbox no longer has this torrent"}

// errAddInFlight — an add for this release is already out and unanswered, so the release IS being
// fetched. Distinct from the hourly budget: this is per hash and per service, says nothing about the
// account, and its honest answer is "coming", not "refused". Reported as a 503 naming the store, the
// tvOS client told the viewer their debrid was refusing and stopped trying other sources — for a release
// scout had itself queued moments earlier.
var errAddInFlight = errors.New("an add for this release is already in flight")

// Store is a debrid backend.
//
// CacheCheck returns ONLY what it learned: a hash it could not check is ABSENT from the map, which the
// pool reads as unknown. Do not pre-seed the map with false — "not cached" and "could not find out" have
// opposite costs, and conflating them drops playable releases from a cached-only list and pays adds for
// torrents the account already holds. The error is non-nil only when NOTHING could be checked (every
// batch failed), which is what tells the pool a store is down rather than empty.
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
	// Status — the HTTP status the service answered with, when there was one. Carried rather than parsed
	// back out of Reason: that string embeds the service's own words, including up to 200 bytes of a
	// non-JSON body, so matching "401"/"403" in it classified a rate-limit detail ("retry in 403 ms") and
	// a Cloudflare Ray ID as a rejected key and silenced a healthy account. Deriving a fact from prose
	// when the fact itself is available at the call site is the same mistake isPermanentFailure was
	// deleted for.
	Status int
}

func (e *StoreUnavailableError) Error() string {
	return "store_unavailable: " + string(e.Service) + " " + e.Reason
}

// readStoreError pulls the service's own explanation out of a failed response, as " (detail)" ready to
// append to a message. TorBox answers `{"error":"ACTIVE_LIMIT","detail":"..."}`; anything unparseable
// falls back to a clipped snippet of the raw body, because a truncated reason still beats none.
func readStoreError(resp *http.Response) string {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if err != nil {
		return ""
	}
	return storeErrorText(raw)
}

// storeErrorText is readStoreError over bytes already in hand, for a caller that had to read the body
// once for its own sake — RD's add answers with the torrent id and the reason for a failure in the same
// place, and decoding straight off the reader left nothing behind to explain a failure with.
func storeErrorText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	if len(raw) > 2048 {
		raw = raw[:2048]
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

// redactToken removes a credential from text that is about to be logged. The empty check is not
// defensive tidiness: strings.ReplaceAll with an empty needle splices the replacement between every
// character, so an unconfigured store would turn a short error into an unreadable one.
func redactToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "<token>")
}

// storeRefusedUs reports whether an HTTP status is the service declining to serve this account right now —
// a throttle or a fault on their side — rather than a verdict about the torrent.
func storeRefusedUs(status int) bool {
	// 401/403 belong here too: an expired or wrong API key is the service declining to serve THIS ACCOUNT,
	// which says nothing about the release. Left out, a stale token produced a plain dead link — so no
	// backoff was recorded, every poll paid another add against scout's own allowance, and the eventual
	// refusals read as scout being busy rather than as a key that needs replacing.
	return status == http.StatusUnauthorized || status == http.StatusForbidden ||
		status == http.StatusTooManyRequests || status >= 500
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
// resolveKey — the cached (torrent id + file list) for a hash on this account. Scoped by the debrid
// token: the value is an account-scoped torrent id, so an infohash-only key would let one user's id be
// used with another user's token.
func resolveKey(token, infoHash string) string {
	return "torbox:resolve:" + keyHash(token) + ":" + infoHash
}

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

// An add we SENT but never got an answer to.
//
// This is the honest state between "queued" and "not queued", and it needs its own memory because the
// two obvious ones both lie about it. Treating it as a refusal blames the service for the client hanging
// up; treating it as never-happened re-adds on the next poll. A client polls /play every couple of
// seconds and cancels on a focus change, so re-adding turned ONE release into sixty createtorrent calls
// against a sixty-an-hour ceiling — measured — while the budget reported a full allowance because each
// one had been refunded.
//
// Long enough to outlast the poll cadence that causes the loop, short enough that a genuinely lost add
// can be retried within the wait rather than at the end of it.
const addAttemptTTL = 90 * time.Second

func addAttemptKey(svc DebridService, token, infoHash string) string {
	return string(svc) + ":adding:" + keyHash(token) + ":" + infoHash
}

// addInFlight reports an add this process sent and never heard back about.
//
// The error wraps errAddInFlight, which handlePlay answers with 202 "downloading": it is scout's own
// bookkeeping, not the service declining, so it must not be written into the refusal memory or reported
// to the client as the debrid's doing — the confusion errScoutSide exists to end.
func addInFlight(cache Cache, svc DebridService, token, infoHash string) error {
	if !addOutcomeUnknown(cache, svc, token, infoHash) {
		return nil
	}
	return fmt.Errorf("%w: %w", errAddInFlight,
		&StoreUnavailableError{Service: svc, Reason: "scout already sent an add for this release and is awaiting the result"})
}

// addOutcomeUnknown reports that an add went out and nothing came back — the marker is written before
// the request and cleared only once a response arrives.
//
// It is what keeps scout's own uncertainty out of the refusal memory. A connection reset mid-add is not
// the service refusing: TorBox's addMagnet says exactly that ("the outcome is genuinely unknown: the
// marker STAYS") and then its caller recorded a refusal anyway, which pre-empted this marker on the next
// poll — backedOff is consulted first there — so errAddInFlight became unreachable and the client was
// told its debrid was refusing, for a release scout had an add out for. Premiumize was right by
// accident: it happens to ask addInFlight before backedOff.
func addOutcomeUnknown(cache Cache, svc DebridService, token, infoHash string) bool {
	if cache == nil {
		return false
	}
	_, inFlight := cache.Get(addAttemptKey(svc, token, infoHash))
	return inFlight
}

// noteAddAttempt records that an add request went out, whatever comes back.
//
// EVERY store that can add needs this, not just the one where the loop was first measured. Giving it to
// TorBox alone did not stop the loop — it moved it: a cancelling client simply spent Real-Debrid's
// allowance instead, fifty adds in under two minutes, because ResolvePreferring walks on to the store
// with no marker.
func noteAddAttempt(cache Cache, svc DebridService, token, infoHash string) {
	if cache == nil {
		return
	}
	cache.Put(addAttemptKey(svc, token, infoHash), "1", addAttemptTTL)
	// Clear the torrent-miss marker too. It suppresses the account listing for 15s, and that listing is
	// the only thing that can discover the torrent this add just created — so leaving it kept the next
	// poll on the "nothing is queued" path instead of "it is downloading". The negative cache and the add
	// loop feed each other. This line was dropped when these helpers were made shared, along with the
	// paragraph saying why, and nothing failed: hence the test that now covers it.
	if svc == ServiceTorBox {
		cache.Put(torrentMissKey(token, infoHash), "", time.Nanosecond)
	}
}

// pmQueuedKey — Premiumize's directdl queued a transfer and had nothing to serve yet.
//
// Set ONLY on that outcome, never on a successful one. directdl IS the fetch: for a release the account
// holds it returns links straight away, and only an uncached one queues. A marker set on every call
// blocked releases that would have played instantly — one infohash is a whole season pack, so the second
// episode of a show could not be resolved for twenty minutes. Marking only the empty answer suppresses
// exactly the repeat that costs something, and nothing else.
func pmQueuedKey(token, infoHash string) string {
	return "premiumize:queued:" + keyHash(token) + ":" + infoHash
}

// Long enough to cover a download, short enough that a transfer which never arrives can be asked for
// again within one sitting.
const queuedTTL = 20 * time.Minute

// How long "the transfer is coming" stays believable. Past this the release is reported dead so the
// client can fall through, while the marker itself lives on to the full queuedTTL so the transfer is not
// queued a second time.
const pendingGiveUp = 10 * time.Minute

// noteQueued stamps the moment the transfer was queued. The TIME, not a flag: a flag rewritten on every
// poll never ages, and a claim that never ages is not a wait, it is a promise nothing can withdraw.
func noteQueued(cache Cache, token, infoHash string) {
	if cache != nil {
		cache.Put(pmQueuedKey(token, infoHash), strconv.FormatInt(time.Now().Unix(), 10), queuedTTL)
	}
}

// pendingTooLong — this transfer was queued long enough ago that "still coming" has stopped being a
// credible answer, so the client should be free to try something else.
func pendingTooLong(cache Cache, token, infoHash string) bool {
	if cache == nil {
		return false
	}
	raw, ok := cache.Get(pmQueuedKey(token, infoHash))
	if !ok {
		return false
	}
	at, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && time.Since(time.Unix(at, 0)) >= pendingGiveUp
}

// settleQueuedTransfer clears the marker once Premiumize has something to serve — the transfer is done,
// and a later request must be free to pay for it again if it is ever evicted.
func settleQueuedTransfer(cache Cache, token, infoHash string) {
	if cache != nil {
		cache.Put(pmQueuedKey(token, infoHash), "", time.Nanosecond)
	}
}

func alreadyQueued(cache Cache, token, infoHash string) bool {
	if cache == nil {
		return false
	}
	_, queued := cache.Get(pmQueuedKey(token, infoHash))
	return queued
}

// settleAddAttempt clears the marker once the outcome IS known, whatever it was.
//
// The marker means "we do not know", not "an add happened" — so it must not outlive learning. A refused
// add has the refusal backoff, a successful one has its cached torrent id, and a torrent discovered in
// the account listing is plainly no longer in flight; all three describe the state better than this
// does. Keeping it would block the legitimate retry when the add succeeded but the follow-up did not.
func settleAddAttempt(cache Cache, svc DebridService, token, infoHash string) {
	if cache != nil {
		cache.Put(addAttemptKey(svc, token, infoHash), "", time.Nanosecond)
		// An outcome, of any kind, ends the run of not knowing.
		cache.Put(unknownOutcomeKey(svc, token, infoHash), "", time.Nanosecond)
	}
}

// unknownOutcomeKey — when scout FIRST failed to learn what an add did, for this release on this
// account. The add-attempt marker cannot answer that: it lives 90 seconds and is written afresh by every
// new attempt, so a release whose add answer is never readable cycles marker → expiry → new charged add
// → marker, forever. The client is shown 202 "downloading" throughout and no path ever says dead link,
// so it cannot fall through; on RD and Premiumize, which have no Status to rediscover the torrent with,
// that is forty duplicate torrents an hour and a viewer with nothing to do but back out.
//
// This is the deadline `pendingGiveUp` already gives Premiumize's queue marker — "'coming' is a claim
// with a deadline… refreshing the marker made it an absorbing state" — applied to the sibling marker
// that never got one.
func unknownOutcomeKey(svc DebridService, token, infoHash string) string {
	return string(svc) + ":unknown:" + keyHash(token) + ":" + infoHash
}

// How long "we do not know yet" stays a wait rather than a verdict. Past it the release reads as dead so
// the client moves on; the memory itself lives longer, so the give-up is not re-litigated every 90s.
const addGiveUp = 10 * time.Minute
const unknownOutcomeTTL = 30 * time.Minute

// noteUnknownOutcome stamps the FIRST such failure and never restamps — a clock that resets is not a
// deadline, which is the mistake noteQueued was fixed for.
func noteUnknownOutcome(cache Cache, svc DebridService, token, infoHash string) {
	if cache == nil {
		return
	}
	key := unknownOutcomeKey(svc, token, infoHash)
	if raw, ok := cache.Get(key); ok && raw != "" {
		return
	}
	cache.Put(key, strconv.FormatInt(time.Now().Unix(), 10), unknownOutcomeTTL)
}

// unknownTooLong reports that the outcome has been unknown for longer than anyone should be asked to wait.
func unknownTooLong(cache Cache, svc DebridService, token, infoHash string) bool {
	if cache == nil {
		return false
	}
	raw, ok := cache.Get(unknownOutcomeKey(svc, token, infoHash))
	if !ok || raw == "" {
		return false
	}
	at, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return false
	}
	return time.Since(time.Unix(at, 0)) > addGiveUp
}

// unknownOutcome is what an add whose answer never arrived returns: "still coming" until the deadline,
// then a dead link so the client can fall through to another release.
func unknownOutcome(cache Cache, svc DebridService, token, infoHash string) error {
	if unknownTooLong(cache, svc, token, infoHash) {
		return &DeadLinkError{string(svc) + " never reported what its add did"}
	}
	noteUnknownOutcome(cache, svc, token, infoHash)
	return fmt.Errorf("%w: %w", errAddInFlight, &StoreUnavailableError{Service: svc,
		Reason: "the add was answered but its body could not be read, so the outcome is unknown"})
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

// isCancellation — the caller went away, rather than the store saying no. Nothing about the store or the
// release can be concluded from it, so it must not be remembered as a refusal.
func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
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
	return backedOff(s.cache, ServiceTorBox, s.token, infoHash)
}

func (s *realDebridStore) RecentRefusal(infoHash string) (string, bool) {
	return backedOff(s.cache, ServiceRealDebrid, s.token, infoHash)
}

func (s *premiumizeStore) RecentRefusal(infoHash string) (string, bool) {
	return backedOff(s.cache, ServicePremiumize, s.token, infoHash)
}

// refusedKey marks a hash the service just declined to add for this account, so polls stop re-asking.
// Per service, because a refusal by one says nothing about another.
func refusedKey(svc DebridService, token, infoHash string) string {
	return string(svc) + ":refused:" + keyHash(token) + ":" + infoHash
}

// accountRefusedKey — the service turned this ACCOUNT away, whatever was asked for.
//
// An expired or wrong API key is not a fact about one release, and remembering it per infohash meant a
// stale token backed off one hash while the next release paid for the same 403 again: sixty releases,
// fifty calls, the whole hourly allowance gone, and replacing the key did not restore service because
// the budget stayed spent for the rest of the hour.
func accountRefusedKey(svc DebridService, token string) string {
	return string(svc) + ":refused-account:" + keyHash(token)
}

// backedOff — this account was turned away a moment ago, for this hash or outright.
func backedOff(cache Cache, svc DebridService, token, infoHash string) (string, bool) {
	if cache == nil {
		return "", false
	}
	if reason, ok := cache.Get(accountRefusedKey(svc, token)); ok {
		return reason, true
	}
	return cache.Get(refusedKey(svc, token, infoHash))
}

// accountBackedOff — the service rejected this ACCOUNT a moment ago, so nothing it is asked will work.
//
// This one DOES gate a read. The per-release add backoff must not (a read cannot have caused it), but a
// rejected key makes every request pointless, including one for a release the account holds — and
// without this the read path recorded the refusal and then asked again on the very next poll.
func accountBackedOff(cache Cache, svc DebridService, token string) (string, bool) {
	if cache == nil {
		return "", false
	}
	return cache.Get(accountRefusedKey(svc, token))
}

// refusalIsAboutTheAccount — a status the service returns about WHO is asking, not about what was asked
// for. Anything else is remembered per release.
func refusalIsAboutTheAccount(err error) bool {
	var unavailable *StoreUnavailableError
	if !errors.As(err, &unavailable) {
		return false
	}
	return unavailable.Status == http.StatusUnauthorized || unavailable.Status == http.StatusForbidden
}

// errScoutSide marks a refusal SCOUT made — its hourly allowance, or an add it already has in flight —
// as opposed to one the service made. It must not be remembered as the store's refusal and must not be
// reported to the client as the store's: saying "torbox refused" when scout did is exactly the confusion
// these guards exist to end, and it condemns an account that is answering perfectly well.
var errScoutSide = errors.New("refused by scout, not by the service")

// scoutSideReason — what scout is busy with, for a client that can only show a sentence. The wrapped
// StoreUnavailableError still carries the wording; only the blame changes.
func scoutSideReason(err error) string {
	var unavailable *StoreUnavailableError
	if errors.As(err, &unavailable) {
		return unavailable.Reason
	}
	return "scout is not accepting this request right now"
}

// recordRefusal remembers a refusal briefly, so a poll loop cannot sustain one.
//
// Only TorBox had this. The other two had no cache at all, so `ResolvePreferring` fell straight through
// to them and added once per poll for the whole length of a download — several hundred adds on Real-
// Debrid for one wait, each leaving a duplicate torrent on the account. The backoff belongs to every
// store that can be refused, not to the one whose refusal was noticed first.
//
// A cancellation is not a refusal: the caller went away, and nothing about the store or the release can
// be concluded from it.
func recordRefusal(cache Cache, svc DebridService, token, infoHash string, err error) {
	// errTorrentGone joins the exclusions: "this account no longer has that torrent" is a fact about the
	// TORRENT, not the service declining. Remembered as a refusal it blocked the re-buy that fact exists
	// to trigger.
	if cache == nil || isCancellation(err) || errors.Is(err, errScoutSide) ||
		errors.Is(err, errAddInFlight) || errors.Is(err, errTorrentGone) {
		return
	}
	if refusalIsAboutTheAccount(err) {
		cache.Put(accountRefusedKey(svc, token), refusalReason(err), refusalBackoff)
		log.Printf("scout: %s refused the account itself (%s) — check the key", svc, refusalReason(err))
		return
	}
	cache.Put(refusedKey(svc, token, infoHash), refusalReason(err), refusalBackoff)
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
	key := resolveKey(s.token, t.InfoHash)

	// A rejected key makes every request pointless, reads included — the same gate RD and Premiumize have.
	// It sits ABOVE the warm fast path, not below it: any pack played in the last six hours resolves
	// straight out of that entry and returns without ever reaching a gate placed after it, which is the
	// normal state during a binge. Ten polls meant ten requestdl calls on a key TorBox had already
	// rejected, and that branch records no refusal either, so it could not even set the key it skipped.
	if reason, ok := accountBackedOff(s.cache, ServiceTorBox, s.token); ok {
		return "", &StoreUnavailableError{Service: ServiceTorBox, Reason: reason + " (backing off)"}
	}

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

	// Does the account ALREADY hold this? The entry above is a six-hour convenience cache, not a record
	// of what the account has — so "not played in the last six hours", which is the normal state of most
	// of a library, fell through to createtorrent and paid an add for a torrent already sitting there.
	// The lookup that answers this properly already exists and is already used by Status; it simply was
	// never consulted here.
	if id, held := s.knownTorrentID(t.InfoHash); held {
		link, err := s.resolveHeldTorrent(ctx, id, key, needFiles, t)
		if err == nil {
			return link, nil
		}
		// The remembered id is a claim with a six-hour life, not a fact — the torrent can be removed from
		// the account at any point, by the user or by TorBox. When it will not resolve, forget it and let
		// the add path below buy it again. Without this the id is not only kept but REFRESHED on every
		// poll, so a deleted torrent answered dead_link for at least six hours and the client blacklisted
		// a release that had been playing an hour earlier. Before this shortcut existed, that case healed
		// itself by falling through to the add.
		// ONLY a definitive "no such torrent" means the remembered id is stale. Anything else — a
		// cancelled poll, a throttle, a timeout, a pack that does not contain the requested episode — is
		// a fact about this attempt, not about what the account holds. Forgetting on all of them cost an
		// add per poll: the re-add re-created the torrent, the next Status found it and cleared the
		// in-flight marker meant to stop the loop, and round it went.
		if !errors.Is(err, errTorrentGone) {
			return "", err
		}
		s.forgetTorrentID(t.InfoHash)
		log.Printf("scout: torbox no longer has %s — re-adding", shortHash(t.InfoHash))
	}

	// From here on, resolving MEANS queueing — so a caller that forbade that is answered now, before the
	// add-backoff below. That backoff describes a state a read-only caller cannot have caused and must
	// not be blocked by.
	if t.NoAdd {
		return "", errWouldAdd
	}

	// Scout's own bookkeeping first, the store's verdict second — the order Premiumize already uses. An
	// add we sent and never heard back about is a 202 "downloading", and asking backedOff ahead of it let
	// any refusal recorded in the meantime answer 503 instead, naming a store that had said nothing.
	if err := addInFlight(s.cache, ServiceTorBox, s.token, t.InfoHash); err != nil {
		return "", err
	}
	// A client polls /play every few seconds for the whole fetch, and every poll that got this far ran a
	// fresh createtorrent. Once TorBox starts throttling, that loop is what keeps it throttled: the add
	// fails, nothing is cached, and the next poll adds again. Back off for a minute after a refusal so a
	// wait costs one call rather than one per poll.
	if s.cache != nil {
		if reason, ok := backedOff(s.cache, ServiceTorBox, s.token, t.InfoHash); ok {
			return "", &StoreUnavailableError{Service: ServiceTorBox, Reason: reason + " (backing off)"}
		}
	}
	torrentID, err := s.addMagnet(ctx, t.InfoHash)
	if err != nil {
		// Every refused add backs off, not only a 429. The refusal that caused the incident was a 400 —
		// TorBox's answer for an account at its download limit — which is a `DeadLinkError`, so keying the
		// backoff on the error TYPE left the one case that mattered re-adding on every poll.
		//
		// A cancelled request is NOT a refusal. /play runs on the client's context and the client cancels
		// aggressively — a focus change is enough — so one cancelled add wrote "context canceled" into the
		// backoff and served that release 503 store_unavailable for the next minute, on both /play and the
		// probe route. An accusation TorBox never made, about a release that is very likely fine.
		//
		// An add that went out and was never answered is not a refusal either — the marker addMagnet
		// deliberately leaves set says the outcome is unknown, and filing that here made backedOff, which
		// this store consults FIRST, pre-empt it on the next poll: errAddInFlight became unreachable and
		// the client was told its debrid was refusing for a release scout had an add out for.
		if !addOutcomeUnknown(s.cache, ServiceTorBox, s.token, t.InfoHash) {
			recordRefusal(s.cache, ServiceTorBox, s.token, t.InfoHash, err)
		}
		return "", err
	}
	link, err := s.resolveHeldTorrent(ctx, torrentID, key, needFiles, t)
	// `gone` about an id createtorrent returned SECONDS ago is TorBox disagreeing with itself, not the
	// account saying it lacks the torrent — and the two must not lead to the same place. Passed through,
	// it left the fresh id unremembered, so the next poll found no known id and added the same torrent
	// again: one add per poll, bounded by nothing but the hourly allowance, which a two-second cadence
	// spends in under two minutes. (The held path above still re-buys on `gone`, which is where that
	// verdict does mean what it says.)
	//
	// Backing off is what actually bounds it — converting the error alone does not, because the next poll
	// finds no known id and buys the torrent again regardless of what this one answered. A store that
	// creates a torrent and then denies holding it is faulting, so naming TorBox is accurate; the answer
	// to THIS poll stays a dead link so the client can move on to another release rather than sit on a
	// service error for a release that may be fine elsewhere.
	if errors.Is(err, errTorrentGone) {
		recordRefusal(s.cache, ServiceTorBox, s.token, t.InfoHash, &StoreUnavailableError{
			Service: ServiceTorBox, Reason: "created this torrent and then denied holding it"})
		return "", &DeadLinkError{"torbox created this torrent and then denied holding it"}
	}
	return link, err
}

// resolveHeldTorrent turns a torrent the account HAS into a playable link: list its files if an episode
// must be picked, remember what was learned, and mint the download. Shared by the just-added path and by
// the one that discovered the torrent was already there — they differ only in how the id was obtained.
func (s *torBoxStore) resolveHeldTorrent(ctx context.Context, torrentID int, key string,
	needFiles bool, t ResolveTarget) (string, error) {
	var files []TorrentFile
	if needFiles {
		var err error
		files, err = s.listFiles(ctx, torrentID)
		if errors.Is(err, errTorrentGone) {
			// Returned HERE, above the id re-stamp below, and left for the caller to act on: this is the
			// account saying it no longer holds the torrent, so the id must be forgotten and the torrent
			// bought again. Stamping it first would refresh a stale id's six hours on every poll.
			return "", err
		}
		// A REJECTED KEY has to be remembered here, or the classification listFiles makes is thrown away
		// by the only caller that sees it: the error was assigned and then never consulted for any value
		// but errTorrentGone, so a 401 fell through to the status-less errNoFileList below and the
		// account-wide backoff never learned the key was dead. Narrow on purpose — a 5xx is this attempt
		// failing, and letting it back the release off would stop the next poll retrying a list that is
		// very likely to succeed.
		if refusalIsAboutTheAccount(err) {
			// The id first, for the same reason the unconditional write below exists: Status needs it to
			// report a download in progress. Returning above that write dropped an id createtorrent had
			// just handed us, so the next poll knew of no torrent and bought another one.
			if s.cache != nil {
				s.cache.Put(torrentIDKey(s.token, t.InfoHash), strconv.Itoa(torrentID), resolveCacheTTL)
			}
			recordRefusal(s.cache, ServiceTorBox, s.token, t.InfoHash, err)
			return "", err
		}
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
	// No list, and an episode to pick out of a pack: refuse rather than guess — but only after the id
	// above is remembered, or a just-queued episode stops reporting as "downloading" and reads as dead.
	//
	// A missing list used to fall straight through to `selectFileID`, where the indexer's raw fileIdx is
	// used as a TorBox file id — the exact disagreement the name-match exists to correct. A blip on
	// mylist therefore served S01E01 for a request for S01E02, with a 302 and no error: silently the
	// wrong episode, and nothing downstream can tell. The cache-read path already refuses on the same
	// evidence (`!needFiles || len(e.Files) > 0`); this is the live path's equivalent. Naming TorBox is
	// accurate — it is the one that did not answer — so the pool moves on to a store that can, and /play
	// answers 503 or, for a torrent still fetching, the 202 `Status` produces from the id just written.
	//
	// Only "no answer" reaches here. `listFiles` returns errTorrentGone above for the account saying it
	// no longer holds the id, which heals by re-adding; answering both alike wedged every episode of a
	// deleted pack at a permanent 503, since the id was re-stamped on each poll and nothing re-added.
	if needFiles && len(files) == 0 {
		return "", errNoFileList
	}
	fileID, err := selectFileID(files, t)
	if err != nil {
		return "", err
	}
	link, err := s.requestDownload(ctx, torrentID, fileID)
	// ONLY a service refusal is remembered here. The add path deliberately records dead links too — a
	// TorBox account at its download limit answers 400, so keying on the error TYPE missed the one case
	// that mattered — but that reasoning does not carry to the read path, where a dead link means "no
	// link yet", the ordinary state of a torrent still downloading. And requestDownload flattens every
	// transport failure into a DeadLinkError on purpose, to keep the token out of the logs, which
	// destroys the classification isCancellation needs: a cancelled poll was being filed as TorBox
	// refusing. Recorded broadly, this told the viewer their debrid was refusing — and stopped the client
	// trying other sources — for a torrent that was merely not ready, and let a read-only probe create a
	// backoff its own contract says it cannot cause.
	var refusedUs *StoreUnavailableError
	if errors.As(err, &refusedUs) {
		recordRefusal(s.cache, ServiceTorBox, s.token, t.InfoHash, err)
	}
	if errors.Is(err, errTorrentGone) {
		// The id we were just handed is not one TorBox has. Undo the two writes above: leaving them
		// re-stamped a stale id with a fresh six-hour TTL on every poll, so a polling client kept its own
		// poison alive and the release stayed unplayable for as long as it kept asking.
		s.forgetTorrentID(t.InfoHash)
	}
	return link, err
}

func (s *torBoxStore) addMagnet(ctx context.Context, infoHash string) (int, error) {
	// An add we already sent and never heard back about must not be sent again — see addAttemptKey.
	if err := addInFlight(s.cache, ServiceTorBox, s.token, infoHash); err != nil {
		return 0, err
	}
	if err := spendAdd(ServiceTorBox, s.token, infoHash); err != nil {
		return 0, err
	}
	form := url.Values{"magnet": {magnetFor(infoHash)}, "seed": {"3"}, "allow_zip": {"false"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+"/torrents/createtorrent", strings.NewReader(form.Encode()))
	if err != nil {
		refundAdd(ServiceTorBox, s.token, errRequestNotSent)
		return 0, err
	}
	req.Header.Set("authorization", "Bearer "+s.token)
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	noteAddAttempt(s.cache, ServiceTorBox, s.token, infoHash)
	resp, err := s.client.Do(req)
	if err != nil {
		// No response, so the outcome is genuinely unknown: the marker STAYS, and the next poll finds it
		// rather than sending the same add again.
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Read the body BEFORE settling, because the read is the last thing that can fail: a status line
	// without a body we could read is still an add whose outcome we never saw. The previous commit added
	// the errAddInFlight branch below but left this settle where it was, so TorBox returned "an add is in
	// flight" having just deleted the marker that says so — and since recordRefusal ignores
	// errAddInFlight there was no backoff either, so the next poll charged and sent createtorrent again.
	// Fifty adds, the hour's allowance gone, the client shown 202 "downloading, 0%" throughout.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxStoreBytes))
	if readErr != nil {
		// Marker left set on purpose: the next poll answers 202 from it rather than buying the torrent
		// again. The charge stays too — the add was written to the wire.
		return 0, unknownOutcome(s.cache, ServiceTorBox, s.token, infoHash)
	}
	settleAddAttempt(s.cache, ServiceTorBox, s.token, infoHash)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// TorBox explains itself in the body — an account at its active-download limit and a malformed
		// magnet are both a bare 400, and only the text says which. Discarding it left the status code as
		// the entire diagnosis, which is how a hard refusal came to look like a slow download.
		//
		// Redacted like the other three detail sites: recordRefusal does not merely log this, it PERSISTS
		// it into the refusal cache, which in production writes through to disk. TorBox's token rides in
		// a header here rather than the URL, so a leak needs the service to echo the header back — but
		// this was the one call site not following the rule, which is reason enough.
		detail := redactToken(storeErrorText(raw), s.token)
		if storeRefusedUs(resp.StatusCode) {
			return 0, &StoreUnavailableError{Service: ServiceTorBox, Status: resp.StatusCode,
				Reason: fmt.Sprintf("createtorrent http %d%s", resp.StatusCode, detail)}
		}
		return 0, &DeadLinkError{fmt.Sprintf("torbox createtorrent http %d%s", resp.StatusCode, detail)}
	}
	var body struct {
		Data *struct {
			TorrentID *int `json:"torrent_id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Data == nil || body.Data.TorrentID == nil {
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

// knownTorrentID answers "does this account already have this torrent?" from cache alone.
//
// Cache-only on purpose. The resolve entry is a six-hour convenience cache, not a record of what the
// account holds, so anything not played within six hours fell through to createtorrent and paid an add
// for a torrent already sitting there. The torrent id answers that — and by the time Resolve runs on the
// /play and probe paths, Status has already looked it up and cached it, so this costs no request at all.
// Asking the account directly here instead would put a full account-list fetch on every cold resolve,
// which is the cost torrentMissKey exists to keep off the poll path.
func (s *torBoxStore) knownTorrentID(infoHash string) (int, bool) {
	if s.cache == nil {
		return 0, false
	}
	raw, ok := s.cache.Get(torrentIDKey(s.token, infoHash))
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(raw)
	return id, err == nil
}

// forgetTorrentID drops both remembered facts about a torrent the account turns out not to have, so the
// next resolve starts from nothing rather than from a stale id.
func (s *torBoxStore) forgetTorrentID(infoHash string) {
	if s.cache == nil {
		return
	}
	s.cache.Put(torrentIDKey(s.token, infoHash), "", time.Nanosecond)
	s.cache.Put(resolveKey(s.token, infoHash), "", time.Nanosecond)
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
	// Finding the torrent settles any add we had in flight for it — that add plainly landed. Without
	// this the marker outlives the fact it stood in for, and a release stays "awaiting the result" long
	// after the result is sitting in the account listing.
	settleAddAttempt(s.cache, ServiceTorBox, s.token, infoHash)
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

// errNoFileList — mylist gave no usable answer. Distinct from errTorrentGone, which is mylist ANSWERING
// that the account no longer holds this id. Collapsing the two is what wedged a stale id: the refusal to
// guess an episode fired before requestDownload, whose 404 was the only producer of errTorrentGone and
// so the only thing that could forget the id and re-add the torrent.
var errNoFileList = &StoreUnavailableError{Service: ServiceTorBox, Reason: "mylist gave no usable answer"}

// listFiles returns the pack's files, or says WHY it cannot. The bare nil it used to answer stood for
// four different facts — a transport blip, a non-200, an unreadable body, and TorBox's own "no such
// torrent" — and every caller had to guess between them.
func (s *torBoxStore) listFiles(ctx context.Context, torrentID int) ([]TorrentFile, error) {
	resp, err := s.get(ctx, fmt.Sprintf("%s/torrents/mylist?id=%d&bypass_cache=true", s.api, torrentID))
	if err != nil {
		return nil, errNoFileList
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errTorrentGone
	}
	// Being turned away is a fact about the ACCOUNT, and errNoFileList carries Status 0 — so a key TorBox
	// has rejected, discovered on this endpoint, could never raise the account-wide backoff that exists
	// to stop every later request asking with the same dead key.
	if storeRefusedUs(resp.StatusCode) {
		return nil, &StoreUnavailableError{Service: ServiceTorBox, Status: resp.StatusCode,
			Reason: fmt.Sprintf("mylist http %d", resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errNoFileList
	}
	var body struct {
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxStoreBytes)).Decode(&body) != nil {
		return nil, errNoFileList
	}
	// TorBox answers 200 with success:false / data:null for an id it no longer holds — the same shape
	// Status already rejects explicitly, and unmarshalling null into a struct succeeds silently. This is
	// mylist ANSWERING, so it is `gone`, not `no answer`: the caller must forget the id and re-add rather
	// than report the store unavailable forever. (On requestdl the same success:false means the opposite
	// — "no link yet" — which is why that endpoint keys the verdict on a 404 instead.)
	if (body.Success != nil && !*body.Success) || string(body.Data) == "null" {
		return nil, errTorrentGone
	}
	// A body carrying no `data` key AT ALL is not the documented envelope — a proxy or CDN page that
	// happens to be valid JSON, say. That is silence, not a claim about the account, and reading it as
	// `gone` paid an add to re-buy a torrent nobody said was missing. The two shapes that really do say
	// so are handled above, so nothing is lost by being careful here.
	if len(body.Data) == 0 {
		return nil, errNoFileList
	}
	type tbFile struct {
		ID        int    `json:"id"`
		Name      string `json:"name"`
		ShortName string `json:"short_name"`
		Size      *int   `json:"size"`
	}
	type entry struct {
		ID    int      `json:"id"`
		Files []tbFile `json:"files"`
	}
	var e entry
	if json.Unmarshal(body.Data, &e) != nil {
		var arr []entry
		// Two different facts, and folding them together reproduced the wedge the errTorrentGone split
		// was written to remove. A payload that will not parse is no answer. An EMPTY ARRAY is TorBox
		// answering: `data:[]` is the array form's only way to say "I hold nothing for this id", exactly
		// as `data:null` is the object form's — and this fallback exists at all because mylist does reply
		// with arrays. Read as "no answer" it kept the stale id, re-stamped its six hours on every poll,
		// and never re-added, so the series path wedged at a permanent 503 while movies still healed.
		if json.Unmarshal(body.Data, &arr) != nil {
			return nil, errNoFileList
		}
		if len(arr) == 0 {
			return nil, errTorrentGone
		}
		// The entry for the id we ASKED about, not whichever came first. mylist is queried by id and so
		// should answer with one, but taking arr[0] blind means a multi-entry answer picks a file id out
		// of another torrent's file list — the wrong-episode failure, by a route the name-match cannot
		// catch, since the names it matches would be the other torrent's.
		e = arr[0]
		if len(arr) > 1 {
			match := -1
			for i, cand := range arr {
				if cand.ID == torrentID {
					match = i
					break
				}
			}
			if match < 0 {
				return nil, errNoFileList
			}
			e = arr[match]
		}
	}
	out := make([]TorrentFile, 0, len(e.Files))
	for _, f := range e.Files {
		name := f.Name
		if name == "" {
			name = f.ShortName
		}
		out = append(out, TorrentFile{Index: f.ID, Name: name, SizeBytes: f.Size})
	}
	return out, nil
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
	// 404 is TorBox saying it has no such torrent — the ONE answer that means a remembered id is stale.
	// Everything else says nothing about whether the account still holds it. Checked before the body is
	// read, because it is the one status whose meaning does not depend on the text.
	if resp.StatusCode == http.StatusNotFound {
		return "", errTorrentGone
	}
	if resp.StatusCode != http.StatusOK {
		// TorBox explains itself in the body, and an account at its active-download limit and a torrent id
		// it cannot serve are both a bare 400. What the text buys here is a DIAGNOSIS, not a
		// classification: 400 is not in `storeRefusedUs`, so such an answer is still a dead link, still
		// records no backoff, and is still re-asked on every poll. That is deliberate on the read path —
		// a dead link means "no link yet", the ordinary state of a torrent still downloading, and
		// requestdl costs no add quota — but it means the operator has to read the log to tell the two
		// apart, and before this the log did not say.
		//
		// Redacted, because this is the ONE TorBox URL that carries the token as a query parameter and
		// an upstream error page may quote the request URI back. The transport branch above goes to
		// lengths to keep that URL out of the logs; a body spliced into the same error would be a second
		// route to the same place, and `docs/SEALED-CONFIG.md` makes "the token is never logged" an
		// acceptance criterion.
		detail := redactToken(readStoreError(resp), s.token)
		if storeRefusedUs(resp.StatusCode) {
			return "", &StoreUnavailableError{Service: ServiceTorBox, Status: resp.StatusCode,
				Reason: fmt.Sprintf("requestdl http %d%s", resp.StatusCode, detail)}
		}
		return "", &DeadLinkError{fmt.Sprintf("torbox requestdl http %d%s", resp.StatusCode, detail)}
	}
	var body struct {
		Success bool   `json:"success"`
		Data    string `json:"data"`
	}
	// NOT errTorrentGone. `success:false` is also what a torrent that is still downloading answers, which
	// is the normal state of one that was queued moments ago — treating it as "the account does not have
	// this" forgot the id of a perfectly good fetch in progress. A 404 is the only answer that means the
	// id is stale; this one means "not yet".
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
	// Below the indexer's position, above the last-resort largest: a number that named several files did
	// not tell them apart, but on a dual-quality pack those several ARE the episode and the better copy
	// wins. Falling straight through to "largest video anywhere" served episode 4 of an eight-file pack
	// for a request for episode 1.
	if t.Season != nil && t.Episode != nil {
		if id := ambiguousEpisodeGuess(files, *t.Episode); id != nil {
			return id, nil
		}
	}
	// An episode was asked for, no filename named one, and the indexer gave no position either: the
	// largest video is the last thing left to go on, and for the common shape — one feature plus a
	// sample — it is right. This used to live inside pickEpisodeFile, where being non-nil made this
	// function return before FileIdx was ever read, so a guess beat a fact. Here it is what it should be:
	// the fallback after the fact rather than instead of it.
	if t.Season != nil && t.Episode != nil {
		if len(episodeFilePool(files)) > 0 {
			idx := largestEpisodeCandidate(files).Index
			return &idx, nil
		}
	}
	return nil, nil
}

// --- Real-Debrid ---

const realDebridAPI = "https://api.real-debrid.com/rest/1.0"

type realDebridStore struct {
	token  string
	client doer
	cache  Cache
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
		ID       int    `json:"id"`
		Path     string `json:"path"`
		Bytes    int    `json:"bytes"`
		Selected int    `json:"selected"`
	} `json:"files"`
	Links []string `json:"links"`
}

// Real-Debrid exposes no queue state we can poll per infohash here, so it never reports a wait — a
// failure stays a failure rather than becoming a promise we can't keep.
func (s *realDebridStore) Status(context.Context, ResolveTarget) (StoreStatus, bool) {
	return StoreStatus{}, false
}

func (s *realDebridStore) Resolve(ctx context.Context, t ResolveTarget) (string, error) {
	// A rejected key makes every request pointless, reads included — see accountBackedOff.
	if reason, ok := accountBackedOff(s.cache, ServiceRealDebrid, s.token); ok {
		return "", &StoreUnavailableError{Service: ServiceRealDebrid, Reason: reason + " (backing off)"}
	}
	// Already bought? Then resolve from it instead of buying it again — BEFORE any add-path guard, the
	// way TorBox orders it. Those guards describe the cost of adding, and this path adds nothing: with
	// the backoff first, one 401 on an unrelated release blocked every release the account already held,
	// read-only probes included, for the whole backoff window.
	if id, held := s.knownTorrent(t); held {
		return s.resolveExisting(ctx, id, t)
	}
	if t.NoAdd {
		return "", errWouldAdd // RD has no cache API, so resolving anything else here is an add
	}
	// The same backoff TorBox has. Without it, a client polling /play for the length of a download added
	// this magnet once per poll — hundreds of adds for one wait, each leaving a duplicate torrent on the
	// account. TorBox backing off correctly made this worse, not better: ResolvePreferring fell straight
	// through to whichever store had no memory of being refused.
	// The same in-flight guard TorBox has, and ahead of the backoff for the same reason: an add of ours
	// that was never answered must read as "coming", not as RD refusing. Giving the guard to TorBox alone
	// did not stop the cancel loop, it moved it: ResolvePreferring walks on to the store without a
	// marker, and fifty cancelled polls shut this account's allowance instead.
	if err := addInFlight(s.cache, ServiceRealDebrid, s.token, t.InfoHash); err != nil {
		return "", err
	}
	if reason, ok := backedOff(s.cache, ServiceRealDebrid, s.token, t.InfoHash); ok {
		return "", &StoreUnavailableError{Service: ServiceRealDebrid, Reason: reason + " (backing off)"}
	}
	if err := spendAdd(ServiceRealDebrid, s.token, t.InfoHash); err != nil {
		return "", err
	}
	noteAddAttempt(s.cache, ServiceRealDebrid, s.token, t.InfoHash)
	addResp, err := s.post(ctx, "/torrents/addMagnet", url.Values{"magnet": {magnetFor(t.InfoHash)}})
	if err != nil {
		// See TorBox's twin: an unanswered add is scout's uncertainty, not RD refusing. RD is the sharp
		// case, having no Status for handlePlay to rescue the answer with.
		if !addOutcomeUnknown(s.cache, ServiceRealDebrid, s.token, t.InfoHash) {
			recordRefusal(s.cache, ServiceRealDebrid, s.token, t.InfoHash, err)
		}
		return "", err
	}
	// Read once, keep the bytes: the id and the explanation of a failure live in the same body, and
	// decoding straight off the reader left nothing for readStoreError further down.
	raw, readErr := io.ReadAll(io.LimitReader(addResp.Body, maxStoreBytes))
	_ = addResp.Body.Close()
	// A body that could not be READ is not an answer that nothing was created. RD had already sent its
	// status line — a 201 means the torrent exists on the account — and a mid-body reset, a TLS
	// truncation or the client's context expiring after the headers arrived all land here. So this is
	// precisely what the in-flight marker is for: an add that went out whose result we never saw. It is
	// settled BELOW, once there is an answer to settle on, so the next poll says "downloading" — honest,
	// and it neither adds again nor blames RD.
	//
	// It must NOT be recorded as a refusal. Doing that meant building a StoreUnavailableError here, which
	// laundered the cause: recordRefusal's isCancellation guard could no longer see it, so a viewer
	// changing focus mid-poll — /play runs on the client's context, and this read is inside it — was
	// filed as Real-Debrid refusing the account, and every /play and probe of that release answered 503
	// store_unavailable for the next minute. RD has no Status for handlePlay to rescue it with. That is
	// the exact confusion the guard, errScoutSide and errAddInFlight all exist to end.
	if readErr != nil {
		return "", unknownOutcome(s.cache, ServiceRealDebrid, s.token, t.InfoHash)
	}
	settleAddAttempt(s.cache, ServiceRealDebrid, s.token, t.InfoHash)
	var added struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &added)
	// A refusal is a fact about the account, not the release — the same distinction TorBox already draws.
	// Every non-2xx here used to become a dead link, so on a Real-Debrid install a 429 or a 503 reached
	// the app as "this release does not exist", and the player walked the whole candidate list collecting
	// the identical non-answer and condemning healthy releases on the way.
	//
	// Either way RD answered and created nothing, so the charge taken before the request goes back — the
	// rule Premiumize's answered-failure branch already followed and this one did not. Without it a
	// magnet RD rejects with a 400, or any 2xx scout cannot pull an id out of (a proxy page served as
	// 200), spent one add per poll: fifty real addMagnet calls inside two minutes at the client's
	// cadence, and then 503 scout_busy on EVERY Real-Debrid resolve for the rest of the rolling hour,
	// healthy releases included. On an RD-only install that is every /play, since RD has no Status for
	// the handler to short-circuit on.
	if addResp.StatusCode < 200 || addResp.StatusCode >= 300 || added.ID == "" {
		refundUnusedAdd(ServiceRealDebrid, s.token)
		detail := redactToken(storeErrorText(raw), s.token)
		if storeRefusedUs(addResp.StatusCode) {
			refused := &StoreUnavailableError{Service: ServiceRealDebrid, Status: addResp.StatusCode,
				Reason: fmt.Sprintf("addmagnet http %d%s", addResp.StatusCode, detail)}
			recordRefusal(s.cache, ServiceRealDebrid, s.token, t.InfoHash, refused)
			return "", refused
		}
		// RD was the only store discarding the service's own words here, so the log said the same thing
		// for a bad magnet, a locked account and a Cloudflare page.
		dead := &DeadLinkError{fmt.Sprintf("realdebrid no torrent id (http %d)%s",
			addResp.StatusCode, detail)}
		// Backed off as well, the way TorBox's add path does. The refund fixes the quota, but on its own
		// it left a poll loop free to re-ask forever — sixty polls, sixty real addMagnet calls — and
		// "free" is why that went unnoticed. The client's answer to THIS poll is still a dead link, so it
		// moves on to another release; only a client that keeps asking about this one meets the backoff.
		recordRefusal(s.cache, ServiceRealDebrid, s.token, t.InfoHash, dead)
		return "", dead
	}
	// Remember the torrent RD just created, the way TorBox remembers its id. This is the fix the marker
	// kept failing to be: a "we already bought this" fact, separate from "an add is in flight", and not
	// cleared when the release resolves or is refused. Without it every poll called addMagnet again and
	// RD made a NEW torrent each time — ten polls of one unplayable pack, ten adds, ten duplicates.
	s.rememberTorrent(t, added.ID)
	id := added.ID

	link, err := s.resolveExisting(ctx, id, t)
	// The same bound TorBox's just-added path has, and for a worse loop. `gone` about a torrent RD
	// created moments ago erases the memory just written, so the next poll finds nothing known and adds
	// again — and RD mints a NEW id every time, so unlike TorBox the re-add cannot converge on one
	// torrent and heal. `recordRefusal` deliberately ignores errTorrentGone, so nothing bounded it: two
	// minutes of one viewer sitting on one release spent the whole hourly allowance, left fifty duplicate
	// torrents on the account, and then answered 503 scout_busy for every RD resolve — healthy releases
	// included — for the rest of the hour. On an RD-only install every /play poll reaches here, because
	// RD has no Status for the handler to short-circuit on.
	if errors.Is(err, errTorrentGone) {
		recordRefusal(s.cache, ServiceRealDebrid, s.token, t.InfoHash, &StoreUnavailableError{
			Service: ServiceRealDebrid, Reason: "created this torrent and then denied holding it"})
		return "", &DeadLinkError{"realdebrid created this torrent and then denied holding it"}
	}
	return link, err
}

// rdTorrentKey — the RD torrent id this account created for an infohash. Account-scoped like every other
// key that carries account state.
// Keyed by the FILE, not the torrent.
//
// One infohash is a whole season pack, and Real-Debrid decides which file a torrent serves through
// selectFiles — the links it then returns describe that selection. So a remembered id is only reusable
// for the same target: reusing it across episodes re-selected on a torrent RD had already started and
// served S01E01's link for a request for S01E02. Wrong content, silently, which is worse than the extra
// add it was saving. A separate torrent per episode is how RD is meant to be used, and repeated polls of
// the SAME episode — the loop that actually costs quota — still reuse one.
func rdTorrentKey(token, infoHash string, t ResolveTarget) string {
	sel := ""
	if t.Season != nil && t.Episode != nil {
		sel = fmt.Sprintf(":s%de%d", *t.Season, *t.Episode)
	} else if t.FileIdx != nil {
		sel = fmt.Sprintf(":f%d", *t.FileIdx)
	}
	return "realdebrid:torrent:" + keyHash(token) + ":" + infoHash + sel
}

func (s *realDebridStore) rememberTorrent(t ResolveTarget, id string) {
	if s.cache != nil {
		s.cache.Put(rdTorrentKey(s.token, t.InfoHash, t), id, resolveCacheTTL)
	}
}

func (s *realDebridStore) knownTorrent(t ResolveTarget) (string, bool) {
	if s.cache == nil {
		return "", false
	}
	id, ok := s.cache.Get(rdTorrentKey(s.token, t.InfoHash, t))
	return id, ok && id != ""
}

func (s *realDebridStore) forgetTorrent(t ResolveTarget) {
	if s.cache != nil {
		s.cache.Put(rdTorrentKey(s.token, t.InfoHash, t), "", time.Nanosecond)
	}
}

// resolveExisting turns an RD torrent this account already has into a playable link. Shared by the
// just-added path and by the one that found it was already bought — they differ only in how the id came.
func (s *realDebridStore) resolveExisting(ctx context.Context, id string, t ResolveTarget) (string, error) {
	info, err := s.info(ctx, id)
	if err != nil {
		// A refusal from the read path is remembered like any other, or nothing ever backs off and the
		// next poll asks again — but ONLY a refusal. `info` also hands back a raw transport error, a bare
		// `realdebrid info http 400`, and `realdebrid bad info` for a non-JSON 200 (a Cloudflare
		// interstitial). None of those is RD declining to serve this account, and filing them as one made
		// a TCP reset answer 503 store_unavailable naming realdebrid — which tells the viewer their
		// debrid is refusing and stops the client trying other sources — for a release RD demonstrably
		// holds. Because this path runs on every poll and each one re-stamps the record, it never lapsed
		// while the blip lasted. The same narrowing TorBox's read path already makes.
		var refusedUs *StoreUnavailableError
		if errors.As(err, &refusedUs) {
			recordRefusal(s.cache, ServiceRealDebrid, s.token, t.InfoHash, err)
		}
		// Only a definitive "no such torrent" forgets the id. An empty file list, a throttle or a timeout
		// all describe this attempt, not what the account holds — forgetting on those re-bought the
		// torrent on the very next poll, which is the loop this memory exists to end.
		if errors.Is(err, errTorrentGone) {
			s.forgetTorrent(t)
		}
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

	sel, err := s.post(ctx, "/torrents/selectFiles/"+id, url.Values{"files": {fmt.Sprintf("%d", *fileID)}})
	if err != nil {
		return "", err
	}
	_ = sel.Body.Close()
	if sel.StatusCode < 200 || sel.StatusCode >= 300 {
		return "", &DeadLinkError{fmt.Sprintf("realdebrid selectFiles http %d", sel.StatusCode)}
	}
	ready, err := s.info(ctx, id)
	if err != nil {
		return "", err
	}
	if len(ready.Links) == 0 {
		return "", &DeadLinkError{"realdebrid not ready"}
	}
	link, ok := rdLinkFor(ready, *fileID)
	if !ok {
		return "", &DeadLinkError{"realdebrid has no link for the selected file"}
	}
	return s.unrestrict(ctx, link)
}

// rdLinkFor picks the link belonging to a file id.
//
// RD's `links` map one-to-one onto the SELECTED files, in file order — not onto every file, and not onto
// the one thing we asked for. Returning links[0] regardless served S01E01's link for a request for
// S01E02 on any torrent with more than one file selected, which is the normal state of a season pack RD
// has already been asked about. Silently the wrong episode, which is worse than any cost it saved.
func rdLinkFor(info *rdInfo, fileID int) (string, bool) {
	pos := 0
	for _, f := range info.Files {
		if f.Selected == 0 {
			continue
		}
		if f.ID == fileID {
			if pos < len(info.Links) {
				return info.Links[pos], true
			}
			return "", false
		}
		pos++
	}
	// No file reports a selection — an older/leaner response shape. With exactly one link there is only
	// one thing it can be; with several, guessing is what caused the bug.
	if pos == 0 && len(info.Links) == 1 {
		return info.Links[0], true
	}
	return "", false
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
	// 404 is RD saying it has no torrent under that id — the one answer that means a remembered id is
	// stale. Anything else, including an empty file list on a torrent whose metadata has not resolved
	// yet, says nothing about whether the account has it.
	if resp.StatusCode == http.StatusNotFound {
		return nil, errTorrentGone
	}
	// A refusal is a fact about the ACCOUNT, not the release — the same rule the add path already
	// follows. Mapping every non-2xx to a dead link on the READ path meant an expired key or a throttle,
	// on a release RD demonstrably holds, reached the app as "this release does not exist": no backoff
	// recorded, and the account backoff shadowed because this path now runs before it.
	if storeRefusedUs(resp.StatusCode) {
		return nil, &StoreUnavailableError{Service: ServiceRealDebrid, Status: resp.StatusCode,
			Reason: fmt.Sprintf("info http %d", resp.StatusCode)}
	}
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
	if t.FileIdx != nil && *t.FileIdx >= 0 && *t.FileIdx < len(files) {
		return &files[*t.FileIdx].Index, nil
	}
	// An index out of range is the indexer describing a file list that is not this one — it arrives
	// unvalidated from the scrape and is never checked against what the debrid actually holds. Falling
	// back to files[0] handed back whatever sorted first, which on a release with a Sample/ directory is
	// the sample: a stream that plays, for one second. The largest is the same guess Premiumize makes and
	// the better one, and it is the answer for "no index at all" here already.
	//
	// Below the indexer's position, above the last-resort largest: a number that named several files did
	// not tell them apart, but on a dual-quality pack those several ARE the episode and the better copy
	// wins. Falling straight through to "largest video anywhere" served episode 4 of an eight-file pack
	// for a request for episode 1.
	if t.Season != nil && t.Episode != nil {
		if id := ambiguousEpisodeGuess(files, *t.Episode); id != nil {
			return id, nil
		}
	}
	//
	// Largest VIDEO, not largest file. pickEpisodeFile used to end in this same fallback over its
	// video-only pool, so this tail was unreachable for a multi-file unlabelled pack; moving the fallback
	// out here exposed it, and a pack whose biggest entry is a .iso or a .rar then resolved to that, with
	// a 302 and no error.
	if t.Season != nil && t.Episode != nil {
		idx := largestEpisodeCandidate(files).Index
		return &idx, nil
	}
	idx := largestPlayable(files).Index
	return &idx, nil
}

// --- Premiumize ---

const premiumizeAPI = "https://www.premiumize.me/api"

type premiumizeStore struct {
	token  string
	client doer
	cache  Cache
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
	// No dedicated account gate here, unlike TorBox and Real-Debrid: Premiumize has no read path at all —
	// directdl is both the question and the purchase — so nothing here reaches the service without first
	// passing the backoff below, and `backedOff` already consults the account key. A separate check would
	// be a no-op wearing the look of a guard. (Two paths DO return above it, `NoAdd` and `addInFlight`,
	// but neither speaks to Premiumize.) TorBox and RD need theirs because their held-torrent paths do
	// talk to the service before any backoff is reached.
	//
	// NoAdd first: it costs nothing and the guards below all describe the cost of adding. TorBox orders
	// it this way for the same reason — a read-only caller cannot have caused a backoff and must not be
	// blocked by one.
	//
	// directdl queues a transfer for anything the account does not already hold, so it is an add.
	if t.NoAdd {
		return "", errWouldAdd
	}
	// The account gate the other two have. It was genuinely redundant here — `backedOff` consults the
	// account key first, and nothing that returned above it spoke to Premiumize — but `addInFlight` now
	// does return above it, so a live marker pre-empted a rejected key and answered 202 "downloading"
	// where TorBox and RD answer 503. A dead key is not a wait.
	if reason, ok := accountBackedOff(s.cache, ServicePremiumize, s.token); ok {
		return "", &StoreUnavailableError{Service: ServicePremiumize, Reason: reason + " (backing off)"}
	}
	if err := addInFlight(s.cache, ServicePremiumize, s.token, t.InfoHash); err != nil {
		return "", err
	}
	// The same backoff TorBox has — a poll loop must not be able to sustain a refusal here either.
	if reason, ok := backedOff(s.cache, ServicePremiumize, s.token, t.InfoHash); ok {
		return "", &StoreUnavailableError{Service: ServicePremiumize, Reason: reason + " (backing off)"}
	}
	// A transfer we already queued. Do NOT skip the call — directdl is the only thing that can discover
	// the transfer finished, so blocking it made a release that completed in two minutes unresolvable for
	// the remaining eighteen. What the marker suppresses is the CHARGE: the transfer is already paid for,
	// so asking about it again must not spend another add.
	queued := alreadyQueued(s.cache, s.token, t.InfoHash)
	// Charged BEFORE the call, because the budget has to be able to gate it — directdl queues a transfer
	// for anything the account lacks, so a spent allowance must stop the request, not merely record it.
	// But directdl is a READ for anything Premiumize already holds, and charging every one of those billed
	// an add per play: twenty-four episodes of a pack the account already had spent twenty-four of the
	// hourly fifty while adding nothing. So the charge is given back below the moment the answer shows
	// nothing was queued.
	if !queued {
		if err := spendAdd(ServicePremiumize, s.token, t.InfoHash); err != nil {
			return "", err
		}
	}
	form := url.Values{"apikey": {s.token}, "src": {magnetFor(t.InfoHash)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+"/transfer/directdl", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	noteAddAttempt(s.cache, ServicePremiumize, s.token, t.InfoHash)
	resp, err := s.client.Do(req)
	if err != nil {
		// The third store gets the same rule. Its ordering already answered 202 from the marker, so the
		// client saw the right thing — but the refusal was still written, and a refusal is read by the
		// probe route and by every other release on this account's per-hash key.
		if !addOutcomeUnknown(s.cache, ServicePremiumize, s.token, t.InfoHash) {
			recordRefusal(s.cache, ServicePremiumize, s.token, t.InfoHash, err)
		}
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	// Read before settling, the same order RD and TorBox use: the read is the last thing that can fail,
	// and a status line whose body we could not read is an add whose outcome we never saw. Premiumize was
	// the store this never reached. Its `!readable` branch REFUNDED the charge and returned a bare dead
	// link, leaving no memory of the add on any of the four channels — charge given back, marker already
	// cleared, noteQueued never reached, no refusal recorded — so thirty polls of one release made thirty
	// real directdl calls with the allowance still at fifty. Nothing bounded it, the budget least of all,
	// because the refund kept handing it back. directdl IS the fetch, so each of those queued a transfer.
	rawBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxStoreBytes))
	if readErr != nil {
		// Marker left set and the charge kept: the next poll answers 202 from the marker instead of
		// buying the transfer again.
		return "", unknownOutcome(s.cache, ServicePremiumize, s.token, t.InfoHash)
	}
	settleAddAttempt(s.cache, ServicePremiumize, s.token, t.InfoHash)
	// Premiumize ANSWERED, and every answer below this line is one that queued nothing — so the charge
	// goes back on all of them, the same rule the success path follows further down. Only the transport
	// failure above keeps its charge, because there the outcome is genuinely unknown.
	//
	// Keeping it was the more expensive mistake by far. The allowance is a rolling in-memory window with
	// no reset but a restart, so a single bad magnet polled every two seconds spent all fifty in about a
	// hundred seconds — after which `spendAdd` refused EVERY Premiumize resolve for the rest of the hour,
	// including releases the account already holds that directdl would have served instantly. A refused
	// magnet is a nuisance; an hour of 503 scout_busy on a working store is an outage.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if !queued {
			refundUnusedAdd(ServicePremiumize, s.token)
		}
		// Redacted for the same reason TorBox's is: the apikey rides in directdl's form body, and this
		// text is not merely logged but PERSISTED into the refusal cache, from where the probe route
		// prints it verbatim. An upstream error page that quotes the request back would put it there.
		detail := redactToken(storeErrorText(rawBody), s.token)
		// Same rule as the other two stores: being turned away says nothing about the release.
		if storeRefusedUs(resp.StatusCode) {
			refused := &StoreUnavailableError{Service: ServicePremiumize, Status: resp.StatusCode,
				Reason: fmt.Sprintf("directdl http %d%s", resp.StatusCode, detail)}
			recordRefusal(s.cache, ServicePremiumize, s.token, t.InfoHash, refused)
			return "", refused
		}
		// Backed off for the same reason RD's twin is: without it a poll loop re-asks a magnet Premiumize
		// has already rejected once every couple of seconds for as long as the viewer sits there. The
		// answer to this poll stays a dead link so the client can fall through to another release.
		dead := &DeadLinkError{fmt.Sprintf("premiumize directdl http %d%s", resp.StatusCode, detail)}
		recordRefusal(s.cache, ServicePremiumize, s.token, t.InfoHash, dead)
		return "", dead
	}
	// Premiumize accepted it. Same reason as RD: no Status endpoint, so without this the next poll
	// queues the same transfer again.
	var body struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Content []struct {
			Path string `json:"path"`
			Link string `json:"link"`
			Size *int   `json:"size"`
		} `json:"content"`
	}
	readable := json.Unmarshal(rawBody, &body) == nil
	// Premiumize answers an application-level refusal as HTTP 200 with `{"status":"error","message":…}` —
	// an unsupported magnet, an account limit, no space. Only ONE of the three ways this call can fail to
	// hand back content means a transfer was queued: a SUCCESSFUL answer that carries none. Collapsing
	// all three charged an add for a transfer that does not exist, stamped the queue marker for its full
	// twenty minutes (suppressing any later legitimate attempt), and answered 202 "downloading, 0%" for
	// ten minutes on a magnet Premiumize had refused outright — a spinner the client cannot fall through.
	if !readable || body.Status != "success" {
		// Nothing was queued, so the charge must go back the same way the read path's does.
		if !queued {
			refundUnusedAdd(ServicePremiumize, s.token)
		}
		// Remembered, like every other answered failure on all three stores. This branch is the one that
		// carries Premiumize's REAL refusals — it reports an unsupported magnet, an account at its limit
		// and "no space" as HTTP 200 with `{"status":"error"}` — and it was the only one recording
		// nothing at all: charge handed back, no marker, no backoff, no queue note. Thirty polls made
		// thirty real directdl calls with the allowance still at fifty, because the refund kept giving it
		// back. Worse for an account out of space, where EVERY release answers this way: twenty healthy
		// releases condemned as dead in sixty calls, and neither /play nor ?probe=1 ever naming
		// Premiumize, because nothing was written for them to read.
		//
		// Still a dead link to the client, though. Its own words — "Invalid src" and "not enough space"
		// need different fixes and only the text says which — and this is not a refusal of the ACCOUNT: a
		// 401/403/429/5xx is what that looks like, and it was handled above.
		var dead *DeadLinkError
		if !readable {
			dead = &DeadLinkError{"premiumize directdl answered something unreadable"}
		} else {
			msg := strings.TrimSpace(body.Message)
			dead = &DeadLinkError{"premiumize directdl: " +
				strings.TrimSpace(body.Status+" "+msg[:min(len(msg), 200)])}
		}
		recordRefusal(s.cache, ServicePremiumize, s.token, t.InfoHash, dead)
		return "", dead
	}
	if len(body.Content) == 0 {
		// A successful answer with nothing in it: this call really did queue a transfer, so the charge
		// stands.
		if !queued {
			noteQueued(s.cache, s.token, t.InfoHash)
		}
		// "Coming" is a claim with a deadline. Refreshing the marker on every poll made it an absorbing
		// state: a dead magnet or a transfer that errored answered 202 at 0% forever, and the client
		// cannot fall through to another release while it is being told one is on its way. Past the
		// window the answer becomes a dead link, which is what it has turned out to be.
		if pendingTooLong(s.cache, s.token, t.InfoHash) {
			// Remembered in the QUEUE marker, not the refusal cache. Filing it as a store refusal made
			// every following poll a 503 naming Premiumize — which tells the viewer their debrid is
			// refusing and stops the client trying other sources, for a release scout itself just
			// condemned, on a store that is answering perfectly well. The whole point of the dead link is
			// that the client moves on; a refusal is the one answer that prevents exactly that.
			return "", &DeadLinkError{"premiumize queued this transfer and never produced anything"}
		}
		return "", fmt.Errorf("%w: %w", errAddInFlight,
			&StoreUnavailableError{Service: ServicePremiumize, Reason: "the transfer is queued and has nothing to serve yet"})
	}
	// Content came back, so the account HOLDS this release: nothing was queued and this call was a read.
	// The refund belongs HERE, before anything can return. Behind the file-selection checks below it was
	// skipped by the two failures a season pack actually produces — the pack does not contain the
	// requested episode, or the chosen entry carries no link — so every poll of such a pack charged
	// another add while queueing nothing. Twenty polls, twenty adds, the hourly allowance gone in under
	// two minutes, on the branch season packs always take.
	//
	// The transfer is likewise no longer pending, whatever we go on to make of its contents.
	if !queued {
		refundUnusedAdd(ServicePremiumize, s.token)
	}
	settleQueuedTransfer(s.cache, s.token, t.InfoHash)

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
	// Below the indexer's position, above the last-resort largest: a number that named several files did
	// not tell them apart, but on a dual-quality pack those several ARE the episode and the better copy
	// wins. Falling straight through to "largest video anywhere" served episode 4 of an eight-file pack
	// for a request for episode 1.
	if t.Season != nil && t.Episode != nil {
		if id := ambiguousEpisodeGuess(files, *t.Episode); id != nil {
			return id, nil
		}
	}
	// Largest VIDEO — see the note on RD's tail: this fallback was unreachable for an unlabelled pack
	// until pickEpisodeFile stopped answering for one, and it picks from every file, .iso included.
	if t.Season != nil && t.Episode != nil {
		idx := largestEpisodeCandidate(files).Index
		return &idx, nil
	}
	idx := largestPlayable(files).Index
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
			stores = append(stores, &realDebridStore{token: token, client: client, cache: cache, api: realDebridAPI})
		case ServicePremiumize:
			stores = append(stores, &premiumizeStore{token: token, client: client, cache: cache, api: premiumizeAPI})
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
	// complete — every configured cache-truth store answered for something. When false one of them is
	// out, and what the others said cannot rule anything out on its behalf.
	complete bool
}

// Cached — at least one store confirmed it holds this.
func (t CacheTruth) Cached(hash string) bool { return len(t.holders[hash]) > 0 }

// Known — at least one cache-truth store gave an answer for this hash, either way. False means unknown,
// which is NOT the same as not cached.
func (t CacheTruth) Known(hash string) bool { return t.known[hash] }

// Complete — every cache-truth store answered. False means one is out, which makes the whole answer
// degraded even where the surviving store's replies look perfectly ordinary.
func (t CacheTruth) Complete() bool { return t.complete }

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
	// Nothing to ask about is a COMPLETE answer, not an outage. Leaving `complete` false here meant a
	// title with no releases at all was reported degraded and re-scraped in full on every request,
	// forever — an outage flag raised over a question nobody asked.
	if len(hashes) == 0 {
		truth.complete = true
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
	answers := make(map[string]int, len(hashes))
	cacheTruthStores, answeredStores := 0, 0
	for i, m := range maps {
		if !isCacheTruthService(p.stores[i].Service()) {
			for hash, cached := range m {
				if cached {
					truth.holders[hash] = append(truth.holders[hash], p.stores[i].Service())
				}
			}
			continue
		}
		cacheTruthStores++
		// A store that answered for NOTHING is out. It STAYS in the denominator below — its silence is
		// exactly why a "no" from the others cannot rule anything out — and the outage is reported
		// separately through Complete, so the handler stops filtering rather than quietly serving an
		// empty list. Dropping it from the denominator instead made the survivor's "no" authoritative: a
		// cachedOnly request came back `{"streams":[]}` with no degraded header, cached for five minutes
		// and held for a day on stale-if-error. A visible no-op is a far better failure than that.
		if len(m) == 0 {
			continue
		}
		answeredStores++
		truthOK = true
		for hash, cached := range m {
			// Presence in the map is the store's claim to have answered for that hash.
			answers[hash]++
			if cached {
				truth.holders[hash] = append(truth.holders[hash], p.stores[i].Service())
			}
		}
	}
	// A "yes" is knowledge on its own: one store confirming it HOLDS a release settles the question, and
	// no other store's silence can unsettle it. A "no" needs everyone who could answer to have answered,
	// because a store that never voted may be the one holding it.
	//
	// The union got the second half wrong — a confident "nobody has this" from the only party that never
	// voted. Requiring unanimity for both got the FIRST half wrong, which was worse: it discarded the
	// only certain facts in the set, so a cached-only list stopped filtering exactly when the surviving
	// store's answers mattered most.
	for hash, n := range answers {
		if len(truth.holders[hash]) > 0 || n == cacheTruthStores {
			truth.known[hash] = true
		}
	}
	truth.complete = answeredStores == cacheTruthStores
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
	// Holders are tried FIRST, which is the whole point: a service that already holds the release serves
	// it now, where another has to fetch it. Every store still gets a turn if the holders fail.
	//
	// An earlier version stopped after one non-holder, to keep a single play from queueing the same
	// torrent on every account. That bound could not be made safe: it bites only when a store FAILS,
	// which is exactly when the next store is the viewer's last chance, so it turned "the first two
	// accounts couldn't serve this" into "you cannot play this" — a release that used to play. Adds are
	// bounded by `spendAdd`, which is a ceiling rather than a coin flip on which store gets to try.
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
