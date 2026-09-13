package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/smhunt/sumpnet/internal/platform"
)

// County of Middlesex open data layers, verified 2026-09-13 (prompt_plan.md §11).
const (
	// AddressLayerURL is the Address point layer of MiddlesexCounty/Base_Layers
	// (native EPSG:26917, maxRecordCount 2000).
	AddressLayerURL = "https://utility.arcgis.com/usrsvcs/servers/f4dd79bdd35a456c87c0c20668138c05/rest/services/MiddlesexCounty/Base_Layers/MapServer/1"
	// RoadLayerURL is the Single Line Road Network (SLRN) polyline layer.
	RoadLayerURL = "https://utility.arcgis.com/usrsvcs/servers/7d7b31fb8cf144939edc0a4436706dfe/rest/services/MiddlesexCounty/SLRN/MapServer/0"
	// Attribution must accompany anything derived from the County data.
	Attribution = "Contains information from the County of Middlesex Open Data portal"
	// LicenceNote records that redistribution terms are unconfirmed (§14).
	LicenceNote = "Licence unconfirmed: the portal item has no licence text and the hub's Terms of Use label is unlinked. Do not commit this data or publish maps derived from it until the County confirms (prompt_plan.md §14)."
)

// Query is an ArcGIS layer query without paging parameters.
type Query struct {
	Where     string
	OutFields []string
	// OrderBy must be a unique field so pages are stable.
	OrderBy string
}

// Params returns the query's parameters (as recorded in the cache): WGS84
// output (outSR 4326), JSON, stable ordering.
func (q Query) Params() map[string]string {
	return map[string]string{
		"where":          q.Where,
		"outFields":      strings.Join(q.OutFields, ","),
		"orderByFields":  q.OrderBy,
		"outSR":          "4326",
		"returnGeometry": "true",
		"f":              "json",
	}
}

// Client queries ArcGIS MapServer layers with paging and retries.
type Client struct {
	HTTP     *http.Client
	PageSize int           // resultRecordCount; default 1000
	MaxPages int           // default 50
	Retries  int           // extra attempts per page on transient errors; default 3
	Backoff  time.Duration // delay before the first retry, doubled each time; default 1s
}

// NewClient returns a client with production defaults.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 60 * time.Second}, PageSize: 1000, MaxPages: 50, Retries: 3, Backoff: time.Second}
}

type arcgisError struct {
	Code    int      `json:"code"`
	Message string   `json:"message"`
	Details []string `json:"details"`
}

type pageHeader struct {
	Features              []json.RawMessage `json:"features"`
	ExceededTransferLimit bool              `json:"exceededTransferLimit"`
	Error                 *arcgisError      `json:"error"`
}

// errTransient marks a failure worth retrying.
type errTransient struct{ err error }

func (e errTransient) Error() string { return e.err.Error() }
func (e errTransient) Unwrap() error { return e.err }

// Fetch runs q against layerURL and returns every raw response page.
// ArcGIS reports query errors as HTTP 200 with an error body; codes 429 and
// 5xx, transport failures and undecodable bodies are retried, other errors
// are not.
func (c *Client) Fetch(ctx context.Context, layerURL string, q Query) ([]json.RawMessage, error) {
	base, err := url.Parse(strings.TrimSuffix(layerURL, "/") + "/query")
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("site: bad layer URL %q", layerURL)
	}
	pageSize, maxPages := c.PageSize, c.MaxPages
	if pageSize <= 0 {
		pageSize = 1000
	}
	if maxPages <= 0 {
		maxPages = 50
	}
	var pages []json.RawMessage
	offset := 0
	for page := 0; ; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("site: %s: more than %d pages", layerURL, maxPages)
		}
		v := url.Values{}
		for k, val := range q.Params() {
			v.Set(k, val)
		}
		v.Set("resultOffset", strconv.Itoa(offset))
		v.Set("resultRecordCount", strconv.Itoa(pageSize))
		u := *base
		u.RawQuery = v.Encode()
		body, hdr, err := c.getWithRetry(ctx, u.String())
		if err != nil {
			return nil, err
		}
		pages = append(pages, body)
		if !hdr.ExceededTransferLimit {
			return pages, nil
		}
		if len(hdr.Features) == 0 {
			return nil, fmt.Errorf("site: %s: exceededTransferLimit with an empty page at offset %d", layerURL, offset)
		}
		offset += len(hdr.Features)
	}
}

func (c *Client) getWithRetry(ctx context.Context, u string) (json.RawMessage, *pageHeader, error) {
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}
	retries := c.Retries
	if retries < 0 {
		retries = 0
	}
	for attempt := 0; ; attempt++ {
		body, hdr, err := c.get(ctx, u)
		if err == nil {
			return body, hdr, nil
		}
		var tr errTransient
		if !errors.As(err, &tr) || attempt >= retries {
			return nil, nil, err
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

func (c *Client) get(ctx context.Context, u string) (json.RawMessage, *pageHeader, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("site: request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sumpnet-dataimport/"+platform.Version+" (+https://github.com/smhunt/sumpnet)")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errTransient{fmt.Errorf("site: GET %s: %w", redact(u), err)}
	}
	defer func() { _ = res.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, nil, errTransient{fmt.Errorf("site: read %s: %w", redact(u), err)}
	}
	if res.StatusCode != http.StatusOK {
		err := fmt.Errorf("site: GET %s: %s: %s", redact(u), res.Status, snippet(raw))
		if res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500 {
			return nil, nil, errTransient{err}
		}
		return nil, nil, err
	}
	var hdr pageHeader
	if err := json.Unmarshal(raw, &hdr); err != nil {
		return nil, nil, errTransient{fmt.Errorf("site: decode %s: %w", redact(u), err)}
	}
	if hdr.Error != nil {
		err := fmt.Errorf("site: ArcGIS error %d from %s: %s %s", hdr.Error.Code, redact(u), hdr.Error.Message, strings.Join(hdr.Error.Details, "; "))
		if hdr.Error.Code == http.StatusTooManyRequests || hdr.Error.Code >= 500 {
			return nil, nil, errTransient{err}
		}
		return nil, nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, nil, errTransient{fmt.Errorf("site: compact %s: %w", redact(u), err)}
	}
	return buf.Bytes(), &hdr, nil
}

// redact keeps the path of a query URL for error messages; the where clause
// names streets, not addresses, but there is no need to repeat it.
func redact(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

func snippet(b []byte) string {
	if len(b) > 256 {
		b = b[:256]
	}
	return strings.TrimSpace(string(b))
}

// sqlString quotes s for an ArcGIS where clause.
func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// AddressQuery selects one street's address points in a municipality.
func AddressQuery(street, munCode string) Query {
	return Query{
		Where:     "FULLSTREET = " + sqlString(street) + " AND MUNCODE = " + sqlString(munCode),
		OutFields: []string{"OBJECTID_1", "GlobalID", "MUNNUMBER", "STREET_NAM", "STREET_TYP", "STREET_DIR", "STREET_UNI", "FULLADDRES", "FULLSTREET", "MUNCODE"},
		OrderBy:   "OBJECTID_1",
	}
}

// RoadQuery selects one road's centreline pieces on either side of a municipality.
func RoadQuery(road, munCode string) Query {
	return Query{
		Where:     "FULLNAME = " + sqlString(road) + " AND (MUNL = " + sqlString(munCode) + " OR MUNR = " + sqlString(munCode) + ")",
		OutFields: []string{"OBJECTID_1", "FULLNAME", "STREET_NAM", "STREET_TYP", "LFADD", "LTADD", "RFADD", "RTADD", "CLASS", "SUBDIVISIO", "PROPOSED", "MUNL", "MUNR"},
		OrderBy:   "OBJECTID_1",
	}
}
