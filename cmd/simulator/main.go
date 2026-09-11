// Command simulator is the sumpnet simulator service. It is a placeholder that only
// serves the platform ops endpoints until its phase in prompt_plan.md §12.
package main

import (
	"context"
	"os"

	"github.com/smhunt/sumpnet/internal/platform"
)

func main() { os.Exit(platform.Run("simulator", run)) }

func run(ctx context.Context, app *platform.App) error {
	app.Ready.Set(true)
	<-ctx.Done()
	return nil
}
