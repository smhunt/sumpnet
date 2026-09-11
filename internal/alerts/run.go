package alerts

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/watermark"
)

// RunOptions collects everything Run needs beyond Config.
type RunOptions struct {
	DSN        string
	GRPCAddr   string
	Notifier   Notifier
	Retry      time.Duration // ALERTS_NOTIFY_RETRY
	MaxAttempt int32         // ALERTS_NOTIFY_MAX_ATTEMPTS
}

// Run is the service body used by cmd/alerts: consumer, notifier loop and
// gRPC server until ctx ends.
func Run(ctx context.Context, app *platform.App, cfg Config, opts RunOptions) error {
	st, err := store.New(ctx, opts.DSN)
	if err != nil {
		return err
	}
	defer st.Close()
	if serr := st.CheckSchema(ctx); serr != nil {
		return fmt.Errorf("schema check (run migrations first): %w", serr)
	}
	if opts.Retry <= 0 {
		opts.Retry = 5 * time.Minute
	}
	if opts.MaxAttempt <= 0 {
		opts.MaxAttempt = 5
	}

	m := NewMetrics(app.Metrics)
	e := NewEngine(cfg, m, app.Log)
	consumer := watermark.New(st.Pool(), opts.DSN, cfg.Watermark, watermark.NewMetrics(app.Metrics), app.Log)
	AddStages(consumer, e)

	wake := make(chan struct{}, 1)
	consumer.OnRound(func(rows int) {
		app.Ready.Set(true)
		if rows > 0 {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		if swerr := sweep(ctx, st, e); swerr != nil {
			app.Log.Warn("offline sweep failed", "err", swerr)
		}
	})

	loop := &NotifierLoop{Queries: st.Queries(), Notifier: opts.Notifier, Metrics: m, Log: app.Log, Interval: 30 * time.Second, Retry: opts.Retry, MaxAttempts: opts.MaxAttempt, Wake: wake}

	srv := grpc.NewServer(
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	alertsv1.RegisterAlertServiceServer(srv, NewServer(st.Queries()))
	reflection.Register(srv)
	lis, err := net.Listen("tcp", opts.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", opts.GRPCAddr, err)
	}
	app.Log.Info("alerts running", "grpc", lis.Addr().String(), "min_severity", SeverityName(cfg.MinSeverity), "notify_resolve", cfg.NotifyResolve, "offline_after", cfg.OfflineAfter)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return consumer.Run(gctx) })
	g.Go(func() error { return loop.Run(gctx) })
	g.Go(func() error {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpc serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
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

// sweep runs the offline sweep in its own transaction.
func sweep(ctx context.Context, st *store.Store, e *Engine) error {
	if e.cfg.OfflineAfter <= 0 {
		return nil
	}
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := e.SweepOffline(ctx, sqlcgen.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
