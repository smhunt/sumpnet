package platform

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// ReadyGate gates /readyz. It starts not-ready; services call Set(true) once
// their dependencies (DB, broker, ...) are connected. RunContext sets it back
// to false at the start of shutdown so load balancers stop routing.
type ReadyGate struct {
	ready atomic.Bool
}

// Set marks the service ready or not ready.
func (g *ReadyGate) Set(v bool) { g.ready.Store(v) }

// Ready reports the current readiness.
func (g *ReadyGate) Ready() bool { return g.ready.Load() }

// ServeHTTP answers 200 when ready and 503 otherwise.
func (g *ReadyGate) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	if !g.Ready() {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// healthz answers 200 whenever the process can serve HTTP at all.
func healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte("ok\n"))
}

// Healthcheck probes /readyz on addr and returns a process exit code (0 ready,
// 1 otherwise). It is what `/svc healthcheck` runs inside the container.
func Healthcheck(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/readyz", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: /readyz returned %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
