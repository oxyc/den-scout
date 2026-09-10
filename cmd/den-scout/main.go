// Command den-scout serves the self-hosted Stremio stream addon.
//
// A single static binary.
package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/oxyc/den-scout/internal/scout"
)

func main() {
	// One plain line per event on stderr. The systemd journal stamps and names every line, so a timestamp
	// or a "den-scout:" prefix here would only print twice.
	log.SetFlags(0)
	settings := scout.SettingsFromEnv(os.Getenv)

	// Pooled keep-alive client so the scrape/debrid fan-out reuses connections.
	client := &http.Client{
		Timeout: 30 * time.Second,
		// A redirect must never replay a request that carries a body: an add is charged once and would
		// otherwise be SENT up to ten times. See RefuseRedirectReplay for the measurements.
		CheckRedirect: scout.RefuseRedirectReplay,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 32, // several users can resolve via the same debrid host at once
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
	cache := scout.NewTieredCache(settings.CacheBytes, settings.CacheDir)
	// Expiry on the disk tier is enforced on READ, so a key nobody asks for again never dies on its own.
	// This sweep is what gives the store a ceiling instead of unbounded growth.
	go cache.SweepEvery(context.Background(), scout.SweepInterval)
	handler := scout.NewHandler(scout.BuildDeps(settings, client, cache))

	srv := &http.Server{
		Addr:              ":" + settings.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Bounds the request line and headers. net/http defaults to 1 MiB, and every cap inside the
		// handler is a PARSE-time cap — by the time decodeConfig refuses an 8 KiB-plus config segment,
		// net/http has already read and buffered the whole megabyte, so the 400 costs the sender nothing
		// and reclaims nothing. Measured: 150 connections sending a 1 MiB request line and withholding
		// the terminating blank line pinned 153 MiB with the handler never invoked, re-established every
		// ReadHeaderTimeout. 16 KiB is twice the config-segment ceiling, which is the largest thing that
		// legitimately appears in a URL here.
		MaxHeaderBytes: 16 << 10,
		// Bounds the whole request read, body included. ReadHeaderTimeout alone covers only the headers:
		// net/http clears the read deadline once they are parsed, so a body arriving one byte per minute
		// held a goroutine and a connection indefinitely. That cost nothing while every route was a GET
		// with no body — /validate is the first one that reads a body, and it is unauthenticated.
		ReadTimeout: 15 * time.Second,
		// Bound the full response so a slow/stuck client (or a slow debrid write) can't pin a goroutine
		// and connection indefinitely. Comfortably above the handler's own list-build/resolve budgets.
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Print(scout.StartupSummary(settings, cache.Persistent()))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := serve(ctx, stop, srv, ln, drainGrace); err != nil {
		log.Fatal(err)
	}
}

// drainGrace is how long in-flight requests get to finish after SIGTERM. It sits under podman's
// default 10s stop timeout, as den-atlas's and den-embed's do, so the drain works whether or not the
// Quadlet's --stop-timeout has reached the box.
const drainGrace = 8 * time.Second

// serve runs srv on ln until ctx is cancelled (SIGTERM or SIGINT), then drains in-flight requests for
// at most grace. Without it a redeploy killed the process outright, cutting every /play mid-resolve.
//
// The bound is the point. Shutdown waits for every active connection, and a client that sends half a
// request and stops would otherwise decide how long a restart takes; running out of time is a
// designed outcome, so it is logged rather than returned. stop is called as the drain starts, which
// restores the default signal action — a second signal kills the process outright.
func serve(ctx context.Context, stop func(), srv *http.Server, ln net.Listener, grace time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	stop()
	log.Printf("shutting down — draining in-flight requests")
	drainCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	if err := srv.Shutdown(drainCtx); err != nil {
		log.Printf("drain deadline (%s) reached with requests still in flight", grace)
		_ = srv.Close()
	}
	return nil
}
