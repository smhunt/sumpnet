// Command ingest receives decoded telemetry from the bridges over gRPC
// (telemetry.v1.IngestService) and stores it idempotently in Postgres.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	telemetryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/telemetry/v1"
	"github.com/smhunt/sumpnet/internal/ingest"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/store"
)

func main() { os.Exit(platform.Run("ingest", run)) }

func run(ctx context.Context, app *platform.App) error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	addr := os.Getenv("GRPC_ADDR")
	if addr == "" {
		addr = ":9090"
	}
	cfg, err := ingest.ConfigFromEnv()
	if err != nil {
		return err
	}
	autoReg, err := ingest.EnvBool("INGEST_AUTO_REGISTER", true)
	if err != nil {
		return err
	}

	st, err := store.New(ctx, dsn, store.WithAutoRegister(autoReg))
	if err != nil {
		return err
	}
	defer st.Close()
	if serr := st.CheckSchema(ctx); serr != nil {
		return fmt.Errorf("schema check (run migrations first): %w", serr)
	}

	metrics := ingest.NewMetrics(app.Metrics)
	st.OnPartitionCreated = func(table string) { metrics.PartitionsCreated.WithLabelValues(table).Inc() }

	srv := grpc.NewServer(
		// Explicit windows keep grpc-go's BDP estimator from inflating per-stream
		// buffers to 16 MiB when a handler stalls; backpressure then bites at ~256 KiB.
		grpc.InitialWindowSize(256<<10),
		grpc.InitialConnWindowSize(1<<20),
		grpc.MaxRecvMsgSize(16<<20),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	)
	telemetryv1.RegisterIngestServiceServer(srv, ingest.New(st, metrics, cfg, app.Log))

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	app.Log.Info("grpc listening", "addr", lis.Addr().String(), "batch_max", cfg.BatchMax, "batch_delay", cfg.BatchDelay, "auto_register", autoReg)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("grpc serve: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		stopped := make(chan struct{})
		go func() {
			srv.GracefulStop()
			close(stopped)
		}()
		select {
		case <-stopped:
		case <-time.After(app.Config.ShutdownTimeout):
			app.Log.Warn("graceful stop timed out; forcing")
			srv.Stop()
		}
		return nil
	})
	app.Ready.Set(true)
	return g.Wait()
}
