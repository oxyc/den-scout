// Command den-scout serves the self-hosted Stremio stream addon.
//
// A single static binary. `den-scout -healthcheck` probes /health and exits 0/1 — the container
// HEALTHCHECK uses it, so no second runtime is spawned (audit #2).
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/oxyc/den-scout/internal/scout"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		healthcheck()
		return
	}

	settings := scout.SettingsFromEnv(os.Getenv)

	// Pooled keep-alive client so the scrape/debrid fan-out reuses connections.
	client := &http.Client{
		Timeout: 30 * time.Second,
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
	log.Printf("den-scout listening on :%s", settings.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func healthcheck() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s/health", net.JoinHostPort("127.0.0.1", port)))
	if err != nil {
		os.Exit(1)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}
