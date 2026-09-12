package auth

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the authenticated caller, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok && p.Subject != ""
}

// RequireOwner returns the caller or an UNAUTHENTICATED status.
func RequireOwner(ctx context.Context) (Principal, error) {
	p, ok := FromContext(ctx)
	if !ok {
		return Principal{}, status.Error(codes.Unauthenticated, "sign in required")
	}
	return p, nil
}

// Authenticate resolves the caller from the `authorization` metadata (which
// grpc-gateway forwards from the HTTP Authorization header). No header means
// an anonymous caller; a header that is not a valid Bearer token is always
// UNAUTHENTICATED, even on public RPCs, so a broken client notices.
func (v *Verifier) Authenticate(ctx context.Context) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	switch len(vals) {
	case 0:
		return ctx, nil
	case 1:
	default:
		return nil, status.Error(codes.Unauthenticated, "multiple authorization values")
	}
	scheme, token, found := strings.Cut(strings.TrimSpace(vals[0]), " ")
	token = strings.TrimSpace(token)
	if !found || !strings.EqualFold(scheme, "bearer") || token == "" {
		return nil, status.Error(codes.Unauthenticated, "authorization must be a Bearer token")
	}
	p, err := v.Verify(ctx, token)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired token")
	}
	return WithPrincipal(ctx, p), nil
}

// UnaryInterceptor authenticates unary calls.
func (v *Verifier) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := v.Authenticate(ctx)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// StreamInterceptor authenticates streaming calls.
func (v *Verifier) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := v.Authenticate(ss.Context())
		if err != nil {
			return err
		}
		return handler(srv, &wrappedStream{ServerStream: ss, ctx: ctx})
	}
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

// Context returns the context carrying the authenticated principal.
func (w *wrappedStream) Context() context.Context { return w.ctx }
