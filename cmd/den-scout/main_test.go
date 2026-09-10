package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// startServe runs serve on a loopback listener with handler, returning the base URL, a cancel that
// stands in for SIGTERM, and the channel serve's result arrives on.
func startServe(t *testing.T, handler http.HandlerFunc, grace time.Duration) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, func() {}, &http.Server{Handler: handler}, ln, grace) }()
	return "http://" + ln.Addr().String(), cancel, done
}

// A request already being handled when SIGTERM lands must complete, not be cut.
func TestServe_anInFlightRequestFinishesAcrossAStop(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	url, stop, done := startServe(t, func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "done")
	}, 5*time.Second)

	got := make(chan string, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			got <- "error: " + err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		got <- string(b)
	}()
	<-started
	stop()
	time.Sleep(50 * time.Millisecond) // let the drain begin while the handler is still running
	close(release)

	if body := <-got; body != "done" {
		t.Fatalf("the in-flight response was cut: %q", body)
	}
	if err := <-done; err != nil {
		t.Fatalf("a clean drain returned %v", err)
	}
}

// A request that never finishes must not hold the stop open past the grace.
func TestServe_theDrainIsBounded(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() { close(release) })
	url, stop, done := startServe(t, func(_ http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
	}, 200*time.Millisecond)

	go func() {
		if resp, err := http.Get(url); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-started
	stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a drain that ran out of time is a designed outcome, not an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the drain is unbounded")
	}
}
