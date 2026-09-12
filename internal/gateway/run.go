// Package gateway is the api-gateway (prompt_plan.md §12 Phase 5, ADR 0006):
// query.v1.QueryService over gRPC and grpc-gateway REST, Clerk owner
// authentication, the WatchNeighbourhood hub, and read-only Postgres access.
// Every public segment view is built with internal/privacy.
package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	queryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/query/v1"
	"github.com/smhunt/sumpnet/internal/auth"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

// Run is the service body used by cmd/api-gateway.
func Run(ctx context.Context, app *platform.App, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	pool, err := OpenReadOnly(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if serr := CheckSchema(ctx, pool); serr != nil {
		return fmt.Errorf("schema check (run migrations first): %w", serr)
	}
	m := NewMetrics(app.Metrics)

	verifier, err := auth.NewVerifier(ctx, cfg.Auth,
		auth.WithHTTPClient(cfg.JWKSClient),
		auth.WithResultHook(func(r string) { m.Auth.WithLabelValues(r).Inc() }))
	if err != nil {
		return err
	}
	if !verifier.Enabled() {
		app.Log.Warn("CLERK_ISSUER is empty: owner authentication is disabled and only public RPCs work")
	}

	var alerts alertsv1.AlertServiceClient
	if cfg.AlertsAddr != "" {
		conn, cerr := grpc.NewClient(cfg.AlertsAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if cerr != nil {
			return fmt.Errorf("alerts client %s: %w", cfg.AlertsAddr, cerr)
		}
		defer func() { _ = conn.Close() }()
		alerts = alertsv1.NewAlertServiceClient(conn)
	} else {
		app.Log.Warn("ALERTS_ADDR is empty: AcknowledgeMyAlert will return UNAVAILABLE")
	}

	hub := NewHub(pool, cfg.DatabaseURL, cfg.Watch, m, app.Log)
	srv := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
		grpc.ChainUnaryInterceptor(verifier.UnaryInterceptor()),
		grpc.ChainStreamInterceptor(verifier.StreamInterceptor()),
	)
	queryv1.RegisterQueryServiceServer(srv, NewServer(sqlcgen.New(pool), alerts, hub, app.Log))
	reflection.Register(srv)
	glis, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.GRPCAddr, err)
	}

	g, gctx := errgroup.WithContext(ctx)
	rest, restConn, err := NewRESTHandler(gctx, loopback(glis.Addr()), cfg.CORSOrigins)
	if err != nil {
		_ = glis.Close()
		return err
	}
	defer func() { _ = restConn.Close() }()

	certFile, keyFile, err := tlsFiles(cfg)
	if err != nil {
		_ = glis.Close()
		return err
	}
	if cfg.TLSCertFile != "" && certFile == "" {
		app.Log.Warn("TLS certificate not loadable; serving REST over plain HTTP (TLS_FALLBACK_HTTP)", "cert", cfg.TLSCertFile)
	}
	restAddr := app.Config.HTTPAddr
	if cfg.RESTAddr == "" {
		app.Mux.Handle("/v1/", rest)
	} else {
		rlis, lerr := net.Listen("tcp", cfg.RESTAddr)
		if lerr != nil {
			_ = glis.Close()
			return fmt.Errorf("listen %s: %w", cfg.RESTAddr, lerr)
		}
		restAddr = rlis.Addr().String()
		top := http.NewServeMux()
		top.Handle("/v1/", rest)
		top.Handle("/", app.Mux) // /healthz, /readyz, /metrics on the public port too
		g.Go(func() error {
			return serveREST(gctx, rlis, top, certFile, keyFile, app.Config.ShutdownTimeout)
		})
	}
	app.Log.Info("api-gateway running", "grpc", glis.Addr().String(), "rest", restAddr,
		"tls", certFile != "", "auth", verifier.Enabled(), "authorized_parties", cfg.Auth.AuthorizedParties,
		"cors_origins", cfg.CORSOrigins, "alerts", cfg.AlertsAddr, "status_window", cfg.Watch.StatusWindow)

	g.Go(func() error { return hub.Run(gctx) })
	g.Go(func() error {
		if err := srv.Serve(glis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpc serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		select {
		case <-hub.Ready():
			app.Ready.Set(true)
		case <-gctx.Done():
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		// hub.Run returning ends every WatchNeighbourhood stream, so the
		// graceful stop does not wait on open subscriptions.
		stopped := make(chan struct{})
		go func() { srv.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(app.Config.ShutdownTimeout):
			srv.Stop()
		}
		return nil
	})
	return g.Wait()
}

// tlsFiles returns the certificate pair to serve, or empty names for plain
// HTTP. A configured pair that cannot be loaded is an error unless
// TLSFallbackHTTP is set.
func tlsFiles(cfg Config) (certFile, keyFile string, err error) {
	if cfg.TLSCertFile == "" {
		return "", "", nil
	}
	if _, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil {
		if cfg.TLSFallbackHTTP {
			return "", "", nil
		}
		return "", "", fmt.Errorf("TLS certificate: %w", err)
	}
	return cfg.TLSCertFile, cfg.TLSKeyFile, nil
}
