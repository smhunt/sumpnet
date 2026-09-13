package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"

	queryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/query/v1"
)

// NewRESTHandler returns the grpc-gateway REST API (JSON; WatchNeighbourhood
// as newline-delimited JSON) wrapped in the CORS allowlist. It proxies to the
// gateway's own gRPC server over loopback, so the auth interceptors, owner
// checks and server streaming behave identically on both transports (the
// in-process grpc-gateway transport cannot stream). grpc-gateway forwards the
// HTTP Authorization header as `authorization` metadata.
func NewRESTHandler(ctx context.Context, grpcTarget string, origins []string) (http.Handler, io.Closer, error) {
	conn, err := grpc.NewClient(grpcTarget, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("rest: dial %s: %w", grpcTarget, err)
	}
	drop := func(string) (string, bool) { return "", false }
	mux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions:   protojson.MarshalOptions{EmitUnpopulated: true},
			UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
		}),
		// gRPC response metadata is internal; do not reflect it into headers.
		runtime.WithOutgoingHeaderMatcher(drop),
		runtime.WithOutgoingTrailerMatcher(drop),
	)
	if err := queryv1.RegisterQueryServiceHandler(ctx, mux, conn); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("rest: register: %w", err)
	}
	return CORS(origins, mux), conn, nil
}

// CORS allows browser calls from an allowlist of exact origins (the
// dashboard, https://dev.ecoworks.ca:3034). Requests without an Origin header
// (curl, or same-origin through the Vite dev proxy) pass through untouched; a
// preflight from any other origin is refused.
func CORS(origins []string, next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
		if !allowed[origin] {
			if preflight {
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r) // no allow header: the browser withholds the response
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		if preflight {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// serveREST serves h on lis until ctx ends, over TLS when certFile/keyFile are
// set. There is no write timeout: WatchNeighbourhood responses are long-lived.
func serveREST(ctx context.Context, lis net.Listener, h http.Handler, certFile, keyFile string, shutdownTimeout time.Duration) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	errc := make(chan error, 1)
	go func() {
		var err error
		if certFile != "" {
			err = srv.ServeTLS(lis, certFile, keyFile)
		} else {
			err = srv.Serve(lis)
		}
		errc <- err
	}()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("rest server: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		_ = srv.Close()
	}
	return nil
}

// loopback turns a wildcard listen address into one this process can dial.
func loopback(addr net.Addr) string {
	if tcp, ok := addr.(*net.TCPAddr); ok && (tcp.IP == nil || tcp.IP.IsUnspecified()) {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(tcp.Port))
	}
	return addr.String()
}
