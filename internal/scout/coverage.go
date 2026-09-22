package scout

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"
)

// A stream list's account of itself, carried in the body beside `streams` as `den`.
//
// An empty `streams` array means two different things — "every source was asked and none has this" and "we
// could not ask" — and a client cannot tell them apart from the array. The envelope says which, and on what
// evidence: `answerKind` names the kind of answer, and `coverage` lists every configured catalogue source with
// what happened when it was asked. Stremio ignores keys it does not know, so a client that reads only
// `streams` behaves exactly as before.
//
// It rides in the body rather than a header because the body is what gets cached: a held list served later
// still carries the time it was built and the coverage it was built from.

const answerEnvelopeVersion = 1

// answerKind values.
const (
	// answerLive: every catalogue source answered and the list has releases.
	answerLive = "live"
	// answerPartial: the list has releases, but at least one source did not answer, so it may be short.
	answerPartial = "partial"
	// answerEmpty: every catalogue source answered and nothing survived. The only kind that asserts a negative.
	answerEmpty = "empty"
	// answerUnknown: nothing to serve, and the pass cannot say why — a source did not answer, or the build is
	// degraded. Never a statement that the title has no releases.
	answerUnknown = "unknown"
	// answerStale: no source answered, so an earlier complete list is served in place of a new one.
	answerStale = "stale"
)

// Source outcomes. A source that did not answer is always one of the non-answered kinds, never absent.
const (
	outcomeAnswered = "answered"
	// outcomeUnreachable: no answer — a network failure, a 5xx, or a config that could not be minted just now.
	outcomeUnreachable = "unreachable"
	// outcomeRefused: the source, or scout's own transport, declined the request (a 4xx, a paused upstream, a
	// household source pointing at a non-public address).
	outcomeRefused = "refused"
	// outcomeTimeout: the source did not answer inside the scrape budget.
	outcomeTimeout = "timeout"
	// outcomeMisconfigured: the source needs a per-install config URL and has none, so it was not asked.
	outcomeMisconfigured = "skipped_misconfigured"
	// outcomeQuarantined: the config names an indexer this build has disabled, so it was not asked.
	outcomeQuarantined = "quarantined"
)

// sourceOutcomes is every outcome, in the order /metrics renders them.
var sourceOutcomes = []string{outcomeAnswered, outcomeUnreachable, outcomeRefused, outcomeTimeout,
	outcomeMisconfigured, outcomeQuarantined}

type answerEnvelope struct {
	V          int    `json:"v"`
	AnswerKind string `json:"answerKind"`
	// Degraded is the X-Den-Degraded value the list was served with, "" when none.
	Degraded    string    `json:"degraded,omitempty"`
	GeneratedAt time.Time `json:"generatedAt"`
	// ExpiresAt is when the list stops being fresh. Absent on a list that is not cached at all.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	Coverage  coverage  `json:"coverage"`
}

type coverage struct {
	// Complete: every catalogue source that could be asked answered. A source that can never be asked —
	// misconfigured or quarantined — does not hold it back, and is listed with that outcome.
	Complete bool           `json:"complete"`
	Sources  []sourceReport `json:"sources"`
}

type sourceReport struct {
	ID string `json:"id"`
	// Label tells a household's own sources apart: they share one id, so each is named by its host.
	Label   string `json:"label,omitempty"`
	Outcome string `json:"outcome"`
	// Items is how many releases the source answered with, before dedupe.
	Items     int   `json:"items"`
	LatencyMS int64 `json:"latencyMs,omitempty"`
	// ObservedAt is when the answer, or the failure, arrived. Absent for a source that was not asked, and for
	// an answer reused from the indexer answer cache, whose age is not recorded.
	ObservedAt time.Time `json:"observedAt,omitzero"`
	// Cached: the answer came from the indexer answer cache rather than a request made for this list.
	Cached bool `json:"cached,omitempty"`
}

// decideAnswerKind is the one place a list's kind is judged. An empty list is `empty` only when every source
// answered and nothing about the build is degraded; any gap makes it `unknown`.
func decideAnswerKind(streams int, complete bool, degraded string) string {
	switch {
	case streams == 0 && (!complete || degraded != ""):
		return answerUnknown
	case streams == 0:
		return answerEmpty
	case !complete:
		return answerPartial
	default:
		return answerLive
	}
}

// excusedFromQuorum reports whether a source's failure to answer leaves the list complete: only when it can
// never be asked, so waiting on it would make completeness unreachable.
func excusedFromQuorum(outcome string) bool {
	return outcome == outcomeMisconfigured || outcome == outcomeQuarantined
}

func reportFor(sc scraper) sourceReport {
	r := sourceReport{ID: string(sc.id())}
	if s, ok := sc.(*stremioScraper); ok {
		r.Label = s.label
	}
	return r
}

// classifyScrape names what one scrape's error means for coverage.
func classifyScrape(sc scraper, err error) string {
	if err == nil {
		return outcomeAnswered
	}
	if u, ok := sc.(unaskableScraper); ok {
		if u.transient {
			return outcomeUnreachable
		}
		return outcomeMisconfigured
	}
	var status *httpStatusError
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return outcomeTimeout
	case errors.Is(err, errNotPublic):
		return outcomeRefused
	case errors.As(err, &status):
		switch {
		case status.code == http.StatusRequestTimeout:
			return outcomeTimeout
		case status.code >= 400 && status.code < 500:
			return outcomeRefused
		}
	}
	return outcomeUnreachable
}

// quarantinedReports lists the indexers a config names that this build has disabled.
func quarantinedReports(config *Config) []sourceReport {
	out := make([]sourceReport, 0, len(config.Quarantined))
	for _, id := range config.Quarantined {
		out = append(out, sourceReport{ID: string(id), Outcome: outcomeQuarantined})
	}
	return out
}

// staleBody relabels a held list served in place of a failed build: `stale`, with the degraded reason the
// response carries. The build time and coverage stay those of the list, which is what makes its age readable.
// A body written before the envelope existed is returned unchanged.
func staleBody(body string) (string, bool) {
	var held struct {
		Streams json.RawMessage `json:"streams"`
		Den     *answerEnvelope `json:"den,omitempty"`
	}
	if json.Unmarshal([]byte(body), &held) != nil || held.Den == nil {
		return body, false
	}
	held.Den.AnswerKind = answerStale
	held.Den.Degraded = "stale_list"
	b, err := json.Marshal(held)
	if err != nil {
		return body, false
	}
	return string(b), true
}
