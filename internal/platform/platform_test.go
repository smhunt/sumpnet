package platform

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T, addr string) Config {
	t.Helper()
	return Config{
		Service:         "test",
		HTTPAddr:        addr,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: 2 * time.Second,
		out:             io.Discard,
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // test-controlled URL
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func TestOpsEndpoints(t *testing.T) {
	cfg := testConfig(t, ":0")
	app := NewApp(cfg, NewLogger(io.Discard, cfg))
	srv := httptest.NewServer(app.Mux)
	defer srv.Close()

	if code, _ := get(t, srv.URL+"/healthz"); code != http.StatusOK {
		t.Errorf("/healthz = %d, want 200", code)
	}
	if code, _ := get(t, srv.URL+"/readyz"); code != http.StatusServiceUnavailable {
		t.Errorf("/readyz before Ready = %d, want 503", code)
	}
	app.Ready.Set(true)
	if code, _ := get(t, srv.URL+"/readyz"); code != http.StatusOK {
		t.Errorf("/readyz after Ready = %d, want 200", code)
	}
	code, body := get(t, srv.URL+"/metrics")
	if code != http.StatusOK {
		t.Errorf("/metrics = %d, want 200", code)
	}
	if !strings.Contains(body, `sumpnet_build_info{commit="unknown",service="test",version="dev"} 1`) {
		t.Errorf("/metrics missing build_info gauge; got:\n%s", body)
	}
}

func TestRunContextStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, testConfig(t, "127.0.0.1:0"), func(ctx context.Context, _ *App) error {
			<-ctx.Done()
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunContext = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunContext did not return after context cancellation")
	}
}

func TestRunContextReturnsWhenFnFinishes(t *testing.T) {
	done := make(chan error, 1)
	go func() {
		done <- RunContext(context.Background(), testConfig(t, "127.0.0.1:0"), func(context.Context, *App) error {
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunContext = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunContext did not return after fn finished")
	}
}

func TestRunContextPropagatesError(t *testing.T) {
	want := errors.New("boom")
	err := RunContext(context.Background(), testConfig(t, "127.0.0.1:0"), func(context.Context, *App) error {
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("RunContext = %v, want wrapped %v", err, want)
	}
}

func TestHealthcheck(t *testing.T) {
	addr := freeAddr(t)
	if got := Healthcheck(addr); got != 1 {
		t.Fatalf("Healthcheck with nothing listening = %d, want 1", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- RunContext(ctx, testConfig(t, addr), func(ctx context.Context, app *App) error {
			app.Ready.Set(true)
			close(ready)
			<-ctx.Done()
			return nil
		})
	}()
	<-ready

	deadline := time.Now().Add(5 * time.Second)
	for Healthcheck(addr) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("Healthcheck never returned 0 while service ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunContext = %v", err)
	}
	if got := Healthcheck(addr); got != 1 {
		t.Fatalf("Healthcheck after shutdown = %d, want 1", got)
	}
}
