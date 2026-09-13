package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

// ECCC station facts, verified against MSC GeoMet on 2026-09-12 (§11):
//   - LONDON CS, CLIMATE_IDENTIFIER 6144478 (STN_ID 10999), 43.03 N 81.15 W,
//     ≈23 km from Timberwalk: hourly data 1994 → present with PRECIP_AMOUNT
//     populated (no nulls 2026-07-01 … 09-12; its 1990s rows are null).
//   - LONDON A, 6144473 (STN_ID 50093), same airport: hourly rows exist but
//     PRECIP_AMOUNT is null throughout — unusable for rain.
//   - No closer station reports hourly data (Ilderton's stations are daily
//     and closed); the next hourly stations with precipitation are 74+ km away.
const (
	DefaultECCCBaseURL = "https://api.weather.gc.ca"
	DefaultECCCStation = "6144478"
	ecccCollection     = "collections/climate-hourly/items"
	ecccTimeLayout     = "2006-01-02T15:04:05"
)

// ECCCConfig configures the ECCC poller.
type ECCCConfig struct {
	Enabled      bool          // WEATHER_ECCC_ENABLED: the off switch (tests and replays turn it off)
	BaseURL      string        // WEATHER_ECCC_URL
	Station      string        // WEATHER_ECCC_STATION: CLIMATE_IDENTIFIER
	PollInterval time.Duration // WEATHER_ECCC_POLL_INTERVAL
	Backfill     time.Duration // WEATHER_ECCC_BACKFILL: window of the first poll
	Lookback     time.Duration // WEATHER_ECCC_LOOKBACK: later polls re-read this much, for late and revised hours
	Timeout      time.Duration // WEATHER_ECCC_TIMEOUT: per HTTP request
	PageSize     int
}

// DefaultECCCConfig is the pilot configuration.
func DefaultECCCConfig() ECCCConfig {
	return ECCCConfig{
		Enabled: true, BaseURL: DefaultECCCBaseURL, Station: DefaultECCCStation,
		PollInterval: time.Hour, Backfill: 7 * 24 * time.Hour, Lookback: 48 * time.Hour,
		Timeout: 30 * time.Second, PageSize: 500,
	}
}

// HourlyPrecip is one ECCC hourly precipitation observation.
//
// Interval semantics (verified): PRECIP_AMOUNT at UTC_DATE is the rain in the
// hour ENDING at UTC_DATE. Summing hourly amounts over (06Z, 06Z] reproduces
// the climate-daily TOTAL_PRECIPITATION on every June–September 2026 day with
// rain in the 06Z boundary hour, and the hour-beginning reading does not
// (e.g. 2026-06-05: daily 8.1 mm, hour-ending sum 8.1, hour-beginning 2.8).
// LOCAL_DATE is local standard time (UTC−5 all year) and is not used.
type HourlyPrecip struct {
	Station string
	HourEnd time.Time
	MM      float64
}

// Row is the rainfall row for the observation: ts is the interval start.
func (h HourlyPrecip) Row() RainRow {
	return RainRow{Start: h.HourEnd.Add(-time.Hour), IntervalS: 3600, MM: h.MM}
}

// ECCCClient reads MSC GeoMet's climate-hourly OGC API collection.
type ECCCClient struct {
	base     *url.URL
	http     *http.Client
	pageSize int
	maxPages int
}

// NewECCCClient builds a client for baseURL (e.g. https://api.weather.gc.ca).
func NewECCCClient(baseURL string, httpClient *http.Client, pageSize int) (*ECCCClient, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("weather: bad ECCC base URL %q", baseURL)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if pageSize <= 0 {
		pageSize = 500
	}
	return &ECCCClient{base: u, http: httpClient, pageSize: pageSize, maxPages: 100}, nil
}

type featureCollection struct {
	Features []struct {
		Properties struct {
			ClimateIdentifier string   `json:"CLIMATE_IDENTIFIER"`
			UTCDate           string   `json:"UTC_DATE"`
			PrecipAmount      *float64 `json:"PRECIP_AMOUNT"`
			PrecipFlag        *string  `json:"PRECIP_AMOUNT_FLAG"`
		} `json:"properties"`
	} `json:"features"`
	Links []struct {
		Rel  string `json:"rel"`
		Href string `json:"href"`
	} `json:"links"`
}

// Hourly returns the station's precipitation for hours ending in (from, to],
// following the collection's next links. Hours without an amount (null, or
// flagged M = missing) are counted in skipped and left out.
func (c *ECCCClient) Hourly(ctx context.Context, station string, from, to time.Time) (obs []HourlyPrecip, skipped int, err error) {
	// The datetime filter applies to LOCAL_DATE; pad a day either side and
	// select on UTC_DATE instead.
	q := url.Values{}
	q.Set("f", "json")
	q.Set("CLIMATE_IDENTIFIER", station)
	q.Set("datetime", from.Add(-24*time.Hour).UTC().Format(ecccTimeLayout)+"Z/"+to.Add(24*time.Hour).UTC().Format(ecccTimeLayout)+"Z")
	q.Set("sortby", "LOCAL_DATE")
	q.Set("limit", strconv.Itoa(c.pageSize))
	next := c.base.JoinPath(ecccCollection)
	next.RawQuery = q.Encode()

	for page := 0; next != nil; page++ {
		if page >= c.maxPages {
			return nil, skipped, fmt.Errorf("weather: ECCC paging exceeded %d pages", c.maxPages)
		}
		fc, err := c.get(ctx, next)
		if err != nil {
			return nil, skipped, err
		}
		for _, f := range fc.Features {
			p := f.Properties
			if p.ClimateIdentifier != station {
				continue
			}
			end, perr := time.ParseInLocation(ecccTimeLayout, p.UTCDate, time.UTC)
			if perr != nil {
				return nil, skipped, fmt.Errorf("weather: ECCC UTC_DATE %q: %w", p.UTCDate, perr)
			}
			if !end.After(from) || end.After(to) {
				continue
			}
			if p.PrecipAmount == nil || *p.PrecipAmount < 0 || (p.PrecipFlag != nil && *p.PrecipFlag == "M") {
				skipped++
				continue
			}
			obs = append(obs, HourlyPrecip{Station: station, HourEnd: end, MM: *p.PrecipAmount})
		}
		next = nil
		for _, l := range fc.Links {
			if l.Rel != "next" {
				continue
			}
			u, perr := url.Parse(l.Href)
			if perr != nil {
				return nil, skipped, fmt.Errorf("weather: ECCC next link %q: %w", l.Href, perr)
			}
			// Stay on the configured host (a proxy or test server rewrites
			// nothing) and keep asking for JSON: GeoMet's next links omit f.
			u.Scheme, u.Host = c.base.Scheme, c.base.Host
			if nq := u.Query(); nq.Get("f") == "" {
				nq.Set("f", "json")
				u.RawQuery = nq.Encode()
			}
			next = u
			break
		}
	}
	return obs, skipped, nil
}

func (c *ECCCClient) get(ctx context.Context, u *url.URL) (*featureCollection, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("weather: ECCC request: %w", err)
	}
	req.Header.Set("Accept", "application/geo+json, application/json")
	req.Header.Set("User-Agent", "sumpnet-weather/"+platform.Version+" (+https://github.com/smhunt/sumpnet)")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("weather: ECCC GET: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return nil, fmt.Errorf("weather: ECCC GET %s: %s: %s", u.Path, res.Status, snippet)
	}
	var fc featureCollection
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&fc); err != nil {
		return nil, fmt.Errorf("weather: ECCC decode: %w", err)
	}
	return &fc, nil
}

// ECCCPoller writes ECCC hourly rainfall into the rainfall table.
type ECCCPoller struct {
	client *ECCCClient
	cfg    ECCCConfig
	pool   *pgxpool.Pool
	m      *Metrics
	log    *slog.Logger
	now    func() time.Time
}

// NewECCCPoller builds a poller.
func NewECCCPoller(client *ECCCClient, cfg ECCCConfig, pool *pgxpool.Pool, m *Metrics, log *slog.Logger) *ECCCPoller {
	return &ECCCPoller{client: client, cfg: cfg, pool: pool, m: m, log: log.With("component", "eccc", "station", cfg.Station), now: time.Now}
}

// PollOnce fetches hours ending in (from, to] and upserts them in one
// transaction. It returns the rows inserted or changed; re-reading unchanged
// hours writes nothing.
func (p *ECCCPoller) PollOnce(ctx context.Context, from, to time.Time) (int, error) {
	obs, skipped, err := p.client.Hourly(ctx, p.cfg.Station, from, to)
	if err != nil {
		p.m.ECCCPolls.WithLabelValues("error").Inc()
		return 0, err
	}
	p.m.ECCCNullHours.Add(float64(skipped))
	n, err := p.store(ctx, obs)
	if err != nil {
		p.m.ECCCPolls.WithLabelValues("error").Inc()
		return 0, err
	}
	p.m.ECCCPolls.WithLabelValues("ok").Inc()
	p.m.ECCCLastSuccess.Set(float64(p.now().Unix()))
	p.m.RainfallRows.WithLabelValues(SourceECCC, "written").Add(float64(n))
	p.m.RainfallRows.WithLabelValues(SourceECCC, "unchanged").Add(float64(len(obs) - n))
	p.log.Debug("ECCC poll", "from", from, "to", to, "hours", len(obs), "skipped", skipped, "written", n)
	return n, nil
}

func (p *ECCCPoller) store(ctx context.Context, obs []HourlyPrecip) (int, error) {
	if len(obs) == 0 {
		return 0, nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("weather: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := sqlcgen.New(tx)
	written := 0
	var minTS, maxTS time.Time
	for _, o := range obs {
		r := o.Row()
		n, err := q.UpsertRainfall(ctx, sqlcgen.UpsertRainfallParams{Source: SourceECCC, StationID: o.Station, Ts: r.Start, IntervalS: r.IntervalS, Mm: r.MM})
		if err != nil {
			return 0, fmt.Errorf("weather: upsert ECCC hour %s: %w", o.HourEnd.Format(time.RFC3339), err)
		}
		if n > 0 {
			written += int(n)
			if minTS.IsZero() || r.Start.Before(minTS) {
				minTS = r.Start
			}
			if o.HourEnd.After(maxTS) {
				maxTS = o.HourEnd
			}
		}
	}
	if written > 0 {
		if err := store.Notify(ctx, tx, store.Notification{Table: "rainfall", N: written, MinTS: minTS, MaxTS: maxTS}); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("weather: commit: %w", err)
	}
	return written, nil
}

// Run polls until ctx ends: the first poll covers Backfill, later ones
// Lookback (ECCC publishes with a lag of hours and revises recent values).
// A failed poll is retried after min(PollInterval, 5 min) over the same window.
func (p *ECCCPoller) Run(ctx context.Context) error {
	from := p.now().Add(-p.cfg.Backfill)
	for {
		now := p.now()
		wait := p.cfg.PollInterval
		if _, err := p.PollOnce(ctx, from, now); err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return nil
			}
			p.log.Warn("ECCC poll failed", "err", err)
			wait = min(wait, 5*time.Minute)
		} else {
			from = now.Add(-p.cfg.Lookback)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}
