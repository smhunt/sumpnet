// Command seed loads the demo neighbourhood (segment polygons, the
// simulator's homes and devices, and optionally a Clerk owner link) into the
// sumpnet database. Run it after migrations and before a simulator replay:
// `make seed [DEMO_OWNER_SUBJECT=user_...]` for the synthetic neighbourhood,
// or `make seed SITE=timberwalk OWNER_ADDRESS="<number> <STREET>"
// DEMO_OWNER_SUBJECT=user_...` for a real-geography site snapshot.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/smhunt/sumpnet/internal/seed"
	"github.com/smhunt/sumpnet/internal/site"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

func main() { os.Exit(run()) }

func run() int {
	def := seed.DefaultOptions()
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	dsn := fs.String("database-url", os.Getenv("DATABASE_URL"), "Postgres DSN (default $DATABASE_URL)")
	seedN := fs.Uint64("seed", def.Seed, "simulator seed the homes derive from (match `make sim SEED`)")
	homes := fs.Int("homes", def.Homes, "simulated homes (match `make sim HOMES`)")
	segments := fs.Int("segments", def.Segments, "simulated segments")
	owner := fs.String("owner-subject", os.Getenv("DEMO_OWNER_SUBJECT"), "Clerk user id to link to one home (default $DEMO_OWNER_SUBJECT)")
	ownerHome := fs.Int("owner-home", envInt("DEMO_OWNER_HOME", 0), "index of the home to link the owner to (synthetic neighbourhood)")
	siteFile := fs.String("site-file", "", "real-geography site snapshot (make site-import); replaces -homes and -segments")
	ownerAddress := fs.String("owner-address", os.Getenv("OWNER_ADDRESS"), `with -site-file: the owner's address, "<number> <STREET>" (default $OWNER_ADDRESS)`)
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "seed: -database-url or DATABASE_URL is required")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	o := seed.Options{Seed: *seedN, Homes: *homes, Segments: *segments, OwnerSubject: *owner, OwnerHome: *ownerHome}
	if *siteFile != "" {
		snap, err := site.LoadSnapshot(*siteFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "seed:", err)
			return 2
		}
		if o.Site, err = site.Build(snap); err != nil {
			fmt.Fprintln(os.Stderr, "seed:", err)
			return 2
		}
		o.OwnerAddress = *ownerAddress
	}
	res, err := apply(ctx, *dsn, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		return 1
	}
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
	return 0
}

func apply(ctx context.Context, dsn string, o seed.Options) (seed.Result, error) {
	st, err := store.New(ctx, dsn)
	if err != nil {
		return seed.Result{}, err
	}
	defer st.Close()
	if serr := st.CheckSchema(ctx); serr != nil {
		return seed.Result{}, fmt.Errorf("run migrations first: %w", serr)
	}
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		return seed.Result{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	res, err := seed.Apply(ctx, sqlcgen.New(tx), o)
	if err != nil {
		return seed.Result{}, err
	}
	return res, tx.Commit(ctx)
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}
