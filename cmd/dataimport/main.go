// Command dataimport fills the gitignored local data caches that real-geography
// and observed-rain simulations read:
//
//	dataimport site -site timberwalk [-refresh | -offline]   # County of Middlesex streets -> data/sites/<site>.json
//	dataimport eccc -from 2026-08-01 -to 2026-09-12           # ECCC hourly rain -> data/rain/eccc-hourly-<station>.json
//	dataimport sites                                          # list the committed site configs
//
// `make site-import SITE=…` and `make eccc-import FROM=… TO=…` wrap it. The
// County data licence is unconfirmed (prompt_plan.md §14): never commit what it
// writes. Summaries go to stdout and never print addresses.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/smhunt/sumpnet/internal/raincache"
	"github.com/smhunt/sumpnet/internal/site"
	"github.com/smhunt/sumpnet/internal/weather"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: dataimport site|eccc|sites [flags]")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch args[0] {
	case "site":
		err = runSite(ctx, args[1:], stdout)
	case "eccc":
		err = runECCC(ctx, args[1:], stdout)
	case "sites":
		p := &printer{w: stdout}
		for _, n := range site.ConfigNames() {
			c, cerr := site.LoadConfig(n)
			if cerr != nil {
				p.err = cerr
				break
			}
			p.f("%-32s %2d streets  %s\n", n, len(c.Streets), c.Description)
		}
		err = p.err
	default:
		_, _ = fmt.Fprintf(stderr, "dataimport: unknown command %q (site, eccc, sites)\n", args[0])
		return 2
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "dataimport:", err)
		return 1
	}
	return 0
}

func runSite(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("site", flag.ContinueOnError)
	name := fs.String("site", "timberwalk", "site config name (dataimport sites lists them)")
	cacheDir := fs.String("cache-dir", filepath.Join("data", "cache", "middlesex"), "raw query cache and identity salt")
	outDir := fs.String("out-dir", filepath.Join("data", "sites"), "snapshot directory")
	refresh := fs.Bool("refresh", false, "re-query the County even when a street is cached")
	offline := fs.Bool("offline", false, "use cached queries only; fail if a street is not cached")
	locate := fs.String("locate", "", `report which segment an address ("<number> <STREET>") is in, without printing it`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := site.LoadConfig(*name)
	if err != nil {
		return err
	}
	salt, created, err := site.LoadOrCreateSalt(filepath.Join(*cacheDir, "identity-salt.hex"))
	if err != nil {
		return err
	}
	p := &printer{w: stdout}
	if created {
		p.f("created a new identity salt in %s (home ids and DevEUIs derive from it; keep it with the cache)\n", *cacheDir)
	}
	src := &site.Source{Client: site.NewClient(), Dir: *cacheDir, Refresh: *refresh, Offline: *offline}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	snap, err := (&site.Importer{Source: src, Salt: salt}).Import(ctx, cfg)
	if err != nil {
		return err
	}
	built, err := site.Build(snap)
	if err != nil {
		return err
	}
	snapPath := filepath.Join(*outDir, cfg.Name+".json")
	if err = snap.Write(snapPath); err != nil {
		return err
	}
	fc, err := built.SegmentsGeoJSON()
	if err != nil {
		return err
	}
	geoPath := filepath.Join(*outDir, cfg.Name+".segments.geojson")
	if err = os.WriteFile(geoPath, append(fc, '\n'), 0o600); err != nil {
		return err
	}

	p.f("site %s: %d streets, %d homes, %d segments\nsnapshot %s\noutlines %s\n%s\n\n",
		cfg.Name, len(snap.Streets), len(built.Homes), len(built.Segments), snapPath, geoPath, site.Attribution)
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	t := &printer{w: tw}
	t.f("street\tpoints\thomes\tdup gid\tdup addr\tunit parents\tother\troad pieces\tproposed\tchains\tcentreline m\taddresses from\troads from\t\n")
	for _, st := range snap.Streets {
		a, r := st.AddressStats, st.RoadStats
		length := 0.0
		for _, ch := range st.Centreline {
			for k := 1; k < len(ch); k++ {
				length += haversine(ch[k-1], ch[k])
			}
		}
		t.f("%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.0f\t%s\t%s\t\n", st.Name, a.Raw, a.Kept, a.DuplicateGlobalID, a.DuplicateAddress,
			a.ParentOfUnits, a.OtherStreet+a.NoGeometry+a.NoNumber, r.Pieces, r.Proposed, len(st.Centreline), length, origin(st.AddressSource), origin(st.RoadSource))
	}
	t.flush(tw)
	p.f("\n")
	tw = tabwriter.NewWriter(stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	t2 := &printer{w: tw}
	t2.f("segment\tkind\thomes\toutside outline\tcentreline m\tname\t\n")
	for _, s := range built.Segments {
		t2.f("%s\t%s\t%d\t%d\t%.0f\t%s\t\n", s.ID, s.Kind, s.Homes, s.HomesOutside, s.LengthM, s.Name)
	}
	t2.flush(tw)
	if *locate != "" {
		i, ok := built.Locate(*locate)
		if !ok {
			return fmt.Errorf("-locate: address not found in site %s", cfg.Name)
		}
		p.f("\n-locate: found, in segment %s\n", built.Segments[built.Homes[i].Segment].ID)
	}
	return errors.Join(p.err, t.err, t2.err)
}

func origin(r site.SourceRef) string {
	if r.FromCache {
		return "cache " + r.FetchedAt.Format("2006-01-02")
	}
	return "fetched"
}

func runECCC(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("eccc", flag.ContinueOnError)
	fromS := fs.String("from", "", "window start: hours ending after it (YYYY-MM-DD = UTC midnight, or RFC 3339)")
	toS := fs.String("to", "", "window end: hours ending at or before it")
	station := fs.String("station", weather.DefaultECCCStation, "ECCC CLIMATE_IDENTIFIER (LONDON CS)")
	baseURL := fs.String("url", weather.DefaultECCCBaseURL, "MSC GeoMet base URL")
	out := fs.String("out", "", "cache file (default data/rain/eccc-hourly-<station>.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	from, err := raincache.ParseTime(*fromS)
	if err != nil {
		return fmt.Errorf("-from: %w", err)
	}
	to, err := raincache.ParseTime(*toS)
	if err != nil {
		return fmt.Errorf("-to: %w", err)
	}
	if !to.After(from) {
		return errors.New("-to must be after -from")
	}
	path := *out
	if path == "" {
		path = raincache.DefaultPath(*station)
	}
	f, err := raincache.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		f, err = raincache.New(*station), nil
	}
	if err != nil {
		return err
	}
	if f.Station != *station {
		return fmt.Errorf("%s holds station %s, not %s", path, f.Station, *station)
	}
	client, err := weather.NewECCCClient(*baseURL, &http.Client{Timeout: 60 * time.Second}, 500)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	obs, skipped, err := client.Hourly(ctx, *station, from, to)
	if err != nil {
		return err
	}
	hours := make([]raincache.Hour, len(obs))
	for i, o := range obs {
		hours[i] = raincache.Hour{End: o.HourEnd, MM: o.MM}
	}
	if err := f.Merge(raincache.Fetch{From: from, To: to, FetchedAt: time.Now(), URL: *baseURL + "/collections/climate-hourly/items", Skipped: skipped}, hours); err != nil {
		return err
	}
	if err := f.Write(path); err != nil {
		return err
	}
	win := f.Window(from, to)
	var total float64
	var last time.Time
	for _, h := range win {
		total += h.MM
		last = h.End
	}
	expected := int(to.Sub(from) / time.Hour)
	p := &printer{w: stdout}
	p.f("station %s: %d hours with an amount (%d expected, %d null/missing skipped), %.1f mm, last hour ending %s\ncache %s\n%s\n\nclimate days (06Z–06Z) with rain:\n",
		*station, len(win), expected, skipped, total, last.Format(time.RFC3339), path, raincache.Attribution)
	for _, d := range raincache.ClimateDays(win) {
		p.f("  %s  %5.1f mm\n", d.Day.Format("2006-01-02"), d.MM)
	}
	return p.err
}

// printer writes formatted output and keeps the first write error.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

func (p *printer) flush(tw *tabwriter.Writer) {
	if err := tw.Flush(); p.err == nil {
		p.err = err
	}
}

// haversine is the distance in metres between two lon/lat points.
func haversine(a, b [2]float64) float64 {
	const r = 6_371_008.8
	rad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLon := rad(b[1]-a[1]), rad(b[0]-a[0])
	h := math.Pow(math.Sin(dLat/2), 2) + math.Cos(rad(a[1]))*math.Cos(rad(b[1]))*math.Pow(math.Sin(dLon/2), 2)
	return 2 * r * math.Asin(math.Sqrt(h))
}
