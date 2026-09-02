package scout

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

const leakToken = "tb-super-secret-token-do-not-log"

// failingDoer reproduces what an http.Client returns on a transport failure: a *url.Error carrying the
// full request URL — including, for TorBox's requestdl, the account token in its query string.
type failingDoer struct{ lastURL string }

func (d *failingDoer) Do(req *http.Request) (*http.Response, error) {
	d.lastURL = req.URL.String()
	return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: fmt.Errorf("dial tcp: i/o timeout")}
}

// The debrid token must never reach an error string.
//
// `docs/SEALED-CONFIG.md` lists "the token is never logged" as an acceptance criterion, and the sealing
// design exists precisely because the URL *is* the credential. TorBox's requestdl takes the token as a
// query parameter, and `*url.Error.Error()` embeds the whole URL — Go redacts userinfo passwords, not
// query strings. Both the resolve log line and the /play error line print %v of this error, so one
// timeout would have written a live token into a rotated, shipped container log.
func TestRequestDownload_transportErrorNeverCarriesTheToken(t *testing.T) {
	doer := &failingDoer{}
	s := &torBoxStore{token: leakToken, client: doer, api: "https://api.torbox.example/v1/api"}

	_, err := s.requestDownload(context.Background(), 42, nil)
	if err == nil {
		t.Fatal("a transport failure must be an error")
	}
	// The token really was on the wire — otherwise this test would pass for the wrong reason.
	if !strings.Contains(doer.lastURL, leakToken) {
		t.Fatalf("precondition failed: the request did not carry the token (%s)", doer.lastURL)
	}
	if strings.Contains(err.Error(), leakToken) {
		t.Fatalf("the token leaked into the error: %v", err)
	}
	// And the cause survives, or the redaction has cost the diagnosis it was meant to preserve.
	if !strings.Contains(err.Error(), "timeout") {
		t.Errorf("the reason was lost along with the URL: %v", err)
	}
}

// transportKind reports the cause without the address.
func TestTransportKind(t *testing.T) {
	timeout := &url.Error{Op: "Get", URL: "https://api.torbox.example/x?token=" + leakToken,
		Err: fmt.Errorf("context deadline exceeded (Client.Timeout exceeded)")}
	if got := transportKind(timeout); strings.Contains(got, leakToken) {
		t.Errorf("url.Error leaked its query string: %s", got)
	}
	cancelled := &url.Error{Op: "Get", URL: "https://x?token=" + leakToken, Err: context.Canceled}
	if got := transportKind(cancelled); got != "cancelled" || strings.Contains(got, leakToken) {
		t.Errorf("cancellation: %s", got)
	}
	if got := transportKind(nil); got != "unknown" {
		t.Errorf("nil: %s", got)
	}
}
