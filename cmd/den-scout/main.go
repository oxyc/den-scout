// Command den-scout serves the self-hosted Stremio stream addon.
//
// A single static binary. `den-scout -healthcheck` probes /health and exits 0/1 — the container
// HEALTHCHECK uses it, so no second runtime is spawned (audit #2).
package main

import (
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
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}
	cache := scout.NewMemoryCache(settings.CacheBytes)
	handler := scout.NewHandler(scout.BuildDeps(settings, client, cache))

	srv := &http.Server{
		Addr:              ":" + settings.Port,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
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
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		os.Exit(1)
	}
	os.Exit(0)
}
