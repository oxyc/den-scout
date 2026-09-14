package scout

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// A household's own sources: Stremio stream addons it configured itself — its own MediaFusion, Comet, or
// Torrentio with its debrid account — named in the install config and scraped beside the indexers.
//
// They exist because the shared indexer instances increasingly answer only with a debrid account attached,
// and minting one server-side (MINT_INDEXER_CONFIGS) sends every household's token to a third party. A
// household that pastes its own addon link sends its token only where it chose to, and the link stays
// sealed inside the install config like the token does.
//
// The URL is the caller's, so the fetch is guarded where it can't be talked around: at dial time, every
// address a connection is about to reach must be public. A hostname that resolves to the LAN, loopback or
// the tailnet is refused however it got there — by DNS, a rebinding answer or a redirect.

// ownSources is the metrics and log name every household source shares. Its hosts are not used as names:
// they are chosen by whoever built the config, so a map keyed by them would grow with traffic.
const ownSources Indexer = "own"

const (
	// Ceiling on sources in one config, and on one source's link. An addon link carries its own config
	// segment: a MediaFusion link measured 1,096 characters with no debrid account in it, and an account
	// only adds to that. Three at the ceiling are what maxConfigBlob is sized to hold.
	maxSources   = 3
	maxSourceURL = 2048
)

// normalizeSources keeps the sources a config may name, as base URLs, in order, without repeats.
func normalizeSources(raw []string) []string {
	var out []string
	for _, s := range raw {
		if n, ok := normalizeSource(s); ok && !containsString(out, n) {
			out = append(out, n)
			if len(out) == maxSources {
				break
			}
		}
	}
	return out
}

// normalizeSource turns a pasted addon link into the base its routes hang off: the `/manifest.json` an
// addon's configure page hands out is dropped, and a `stremio://` install link read as the https address
// it stands for. Anything but a plain https URL is refused — no credentials, query or fragment, which a
// stream path would not keep — and so is a host that is literally a non-public address. The dial guard
// is the real defence; this keeps a link that can never work out of the config.
func normalizeSource(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, "stremio://"); ok {
		s = "https://" + rest
	}
	s = strings.TrimRight(strings.TrimSuffix(s, "/manifest.json"), "/")
	if s == "" || len(s) > maxSourceURL {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || u.Opaque != "" {
		return "", false
	}
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil && !publicAddr(ip) {
		return "", false
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", false
	}
	return s, true
}

func sourceHost(base string) string {
	if u, err := url.Parse(base); err == nil {
		return u.Hostname()
	}
	return string(ownSources)
}

// Address space a household source may not reach, beyond what netip already classifies as loopback,
// link-local, multicast or private.
var nonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT — and the tailnet, which is where den lives
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64, which reaches any IPv4 address, private ones included
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64
	netip.MustParsePrefix("2002::/16"),      // 6to4, which embeds an IPv4 address
	netip.MustParsePrefix("fec0::/10"),      // deprecated site-local
}

func publicAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	for _, p := range nonPublicPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

var errNotPublic = errors.New("household source: refusing a non-public address")

// refusePrivateDial runs after DNS and before connect, on the address actually dialled, so no answer to
// the name lookup and no redirect can reach past it.
func refusePrivateDial(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil || !publicAddr(ap.Addr()) {
		return errNotPublic
	}
	return nil
}

// newSourceClient is the client household sources are fetched with. No proxy: a proxy is dialled in the
// source's place, which would put the guard in front of the proxy instead of the source. Redirects stay on
// https and stop after three hops.
func newSourceClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: refusePrivateDial}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext:         dialer.DialContext,
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" || len(via) >= 3 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// Paced like the indexers, per host, but with a ceiling on how many hosts it remembers: the hosts come from
// configs anyone can build, and indexerLimiter keeps a bucket per host forever.
var sourceLimiter = newBoundedHostLimiter(time.Second, 30, 4096)
