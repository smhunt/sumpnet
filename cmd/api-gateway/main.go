// Command api-gateway serves query.v1.QueryService over gRPC and REST
// (grpc-gateway, optional TLS), verifies Clerk owner tokens and streams live
// neighbourhood updates. Configuration: internal/gateway.ConfigFromEnv.
package main

import (
	"context"
	"os"

	"github.com/smhunt/sumpnet/internal/gateway"
	"github.com/smhunt/sumpnet/internal/platform"
)

func main() { os.Exit(platform.Run("api-gateway", run)) }

func run(ctx context.Context, app *platform.App) error {
	cfg, err := gateway.ConfigFromEnv()
	if err != nil {
		return err
	}
	return gateway.Run(ctx, app, cfg)
}
