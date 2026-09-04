package scout

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Checking a debrid token at the moment it is pasted, rather than at the moment someone tries to watch
// something.
//
// A mistyped key was previously indistinguishable from every other reason a stream might not play: the
// configure page checked only that the field was non-empty, and the first evidence of a bad token was an
// empty list or a 503 hours later, on a device with no way to show why.
//
// A standalone function rather than a method on Store, deliberately. Store is the resolve pipeline — its
// implementations carry a cache, an add budget and backoff state, all of which exist to protect an
// account that is already known to work. Validation is one unauthenticated read with none of that around
// it, and widening the interface would have every store, and every test double, grow a method that the
// pipeline never calls.
type validateResult struct {
	Valid bool `json:"valid"`
	// Why not, in words a configure page can show. Never the upstream body: it is not ours to relay and
	// may carry account detail.
	Reason string `json:"reason,omitempty"`
}

// Paced per upstream host. This route takes a token and reports whether it works, which is the shape of a
// credential-stuffing oracle even though the caller must already hold the token to use it — so the thing
// worth protecting is the debrid provider, and pacing the outbound side is what does that. Queueing
// rather than refusing, as everywhere else here: one person clicking "test" never waits.
var validateLimiter = newHostLimiter(2*time.Second, 5)

// How long the whole check may take. One upstream read; a configure page is waiting on it.
const validateBudget = 10 * time.Second

// maxValidateBody bounds the request body. A token is at most 512 bytes (validateConfig enforces that),
// so this is generous for the JSON around one.
const maxValidateBody = 4 << 10

func (h *handler) handleValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, errBody("method_not_allowed"), noStore)
		return
	}
	var body struct {
		Service string `json:"service"`
		Token   string `json:"token"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, maxValidateBody)).Decode(&body) != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad_request"), noStore)
		return
	}
	// Whitelisted the same way validateConfig does it, and the same length ceiling, so this route cannot
	// be used to send arbitrary strings to an upstream.
	if !isDebridService(body.Service) || body.Token == "" || len(body.Token) > 512 {
		writeJSON(w, http.StatusBadRequest, errBody("bad_request"), noStore)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), validateBudget)
	defer cancel()

	// NOTHING here logs the token, and no log line names this route with its arguments. The configure
	// page seals the config in the browser, so this is the one request in the system where the server is
	// handed a token in the clear — it is used once, for one upstream read, and not written anywhere.
	res := validateDebridToken(ctx, h.deps.ProbeClient, DebridService(body.Service), body.Token)
	writeJSON(w, http.StatusOK, res, noStore)
}

// validateDebridToken asks the service whether this token identifies an account. A transport failure is
// reported as "could not check", never as invalid — telling someone their key is wrong because a provider
// was down is worse than telling them nothing.
func validateDebridToken(ctx context.Context, client doer, svc DebridService, token string) validateResult {
	if client == nil {
		return validateResult{Reason: "validation unavailable"}
	}
	u, req, err := validateRequest(ctx, svc, token)
	if err != nil {
		return validateResult{Reason: "validation unavailable"}
	}
	if parsed, perr := url.Parse(u); perr == nil {
		validateLimiter.wait(ctx, parsed.Host)
	}
	if err := ctx.Err(); err != nil {
		return validateResult{Reason: "timed out"}
	}
	resp, err := client.Do(req)
	if err != nil {
		// The indexer scrapers log the service name and never the URL, because MediaFusion's carries an
		// encrypted config. Same rule here for a different reason: the URL would be fine, the token
		// would not, and the two travel together.
		return validateResult{Reason: "could not reach " + string(svc)}
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return validateResult{Reason: "the service rejected this token"}
	case resp.StatusCode != http.StatusOK:
		return validateResult{Reason: fmt.Sprintf("%s answered %d", svc, resp.StatusCode)}
	}
	// Premiumize answers 200 for a bad key and puts the verdict in the body, so the status alone is not
	// the answer for every service.
	if svc == ServicePremiumize {
		var pm struct {
			Status string `json:"status"`
		}
		if json.NewDecoder(http.MaxBytesReader(nil, resp.Body, maxValidateBody)).Decode(&pm) != nil {
			return validateResult{Reason: "could not read the reply"}
		}
		if !strings.EqualFold(pm.Status, "success") {
			return validateResult{Reason: "the service rejected this token"}
		}
	}
	return validateResult{Valid: true}
}

// validateRequest builds the per-service account read. Each is the cheapest authenticated GET the
// service offers — enough to prove the token identifies an account, and nothing more.
func validateRequest(ctx context.Context, svc DebridService, token string) (string, *http.Request, error) {
	var u string
	switch svc {
	case ServiceTorBox:
		u = torboxAPI + "/user/me"
	case ServiceRealDebrid:
		u = realDebridAPI + "/user"
	case ServicePremiumize:
		u = premiumizeAPI + "/account/info?apikey=" + url.QueryEscape(token)
	default:
		return "", nil, fmt.Errorf("unknown service %q", svc)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("accept", "application/json")
	// Premiumize authenticates in the query string, which is already set above.
	if svc != ServicePremiumize {
		req.Header.Set("authorization", "Bearer "+token)
	}
	return u, req, nil
}
