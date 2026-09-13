package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// geomet serves the recorded GeoMet responses in testdata (captured from
// api.weather.gc.ca on 2026-09-12). Their next links still point at the real
// API; the client must rebase them onto the configured host.
func geomet(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.RequestURI())
		mu.Unlock()
		q := r.URL.Query()
		if r.URL.Path != "/collections/climate-hourly/items" || q.Get("f") != "json" || q.Get("sortby") != "LOCAL_DATE" || q.Get("limit") == "" || !strings.Contains(q.Get("datetime"), "/") {
			http.Error(w, "unexpected request "+r.URL.RequestURI(), http.StatusBadRequest)
			return
		}
		var file string
		switch {
		case q.Get("CLIMATE_IDENTIFIER") == "6144478" && q.Get("offset") == "":
			file = "geomet-6144478-page1.json"
		case q.Get("CLIMATE_IDENTIFIER") == "6144478" && q.Get("offset") == "6":
			file = "geomet-6144478-page2.json"
		case q.Get("CLIMATE_IDENTIFIER") == "6144473":
			file = "geomet-6144473-null-precip.json"
		case q.Get("CLIMATE_IDENTIFIER") == "500":
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		default:
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile("testdata/" + file)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/geo+json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestECCCHourlyPagesAndMapsHourEnding(t *testing.T) {
	srv, seen := geomet(t)
	c, err := NewECCCClient(srv.URL, srv.Client(), 6)
	if err != nil {
		t.Fatal(err)
	}
	obs, skipped, err := c.Hourly(context.Background(), "6144478", utc("2026-07-18T13:00:00Z"), utc("2026-07-18T21:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(*seen) != 2 {
		t.Fatalf("requests = %v, want two pages", *seen)
	}
	if !strings.Contains((*seen)[0], "CLIMATE_IDENTIFIER=6144478") || !strings.Contains((*seen)[0], "datetime=2026-07-17T13%3A00%3A00Z%2F2026-07-19T21%3A00%3A00Z") {
		t.Errorf("first request %s", (*seen)[0])
	}
	// Hours ending in (13:00, 21:00]: 14:00 … 21:00 = 8 hours across both pages.
	if len(obs) != 8 || skipped != 0 {
		t.Fatalf("obs = %+v, skipped %d", obs, skipped)
	}
	var total float64
	for _, o := range obs {
		total += o.MM
	}
	wet := obs[4] // UTC_DATE 18:00 carries 9.3 mm
	if !wet.HourEnd.Equal(utc("2026-07-18T18:00:00Z")) || wet.MM != 9.3 || total != 9.3+1.1 {
		t.Errorf("wet hour %+v, total %.1f", wet, total)
	}
	r := wet.Row()
	if !r.Start.Equal(utc("2026-07-18T17:00:00Z")) || r.IntervalS != 3600 || r.MM != 9.3 {
		t.Errorf("row %+v: ts must be the start of the hour ending at UTC_DATE", r)
	}
}

func TestECCCHourlyNullsAndErrors(t *testing.T) {
	srv, _ := geomet(t)
	c, err := NewECCCClient(srv.URL, srv.Client(), 10)
	if err != nil {
		t.Fatal(err)
	}
	obs, skipped, err := c.Hourly(context.Background(), "6144473", utc("2026-07-18T12:00:00Z"), utc("2026-07-18T20:00:00Z"))
	if err != nil || len(obs) != 0 || skipped != 3 {
		t.Errorf("LONDON A: obs %v skipped %d err %v (its PRECIP_AMOUNT is always null)", obs, skipped, err)
	}
	if _, _, err := c.Hourly(context.Background(), "500", utc("2026-07-18T12:00:00Z"), utc("2026-07-18T20:00:00Z")); err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("server error: %v", err)
	}
	if _, err := NewECCCClient("not a url", nil, 0); err == nil {
		t.Error("bad base URL accepted")
	}
}

func TestConfigFromEnv(t *testing.T) {
	c, err := ConfigFromEnv()
	if err != nil || !c.ECCC.Enabled || c.ECCC.Station != DefaultECCCStation || c.ECCC.BaseURL != DefaultECCCBaseURL || c.Watermark.Consumer != "weather" {
		t.Fatalf("defaults = %+v, %v", c, err)
	}
	t.Setenv("WEATHER_ECCC_ENABLED", "false")
	t.Setenv("WEATHER_ECCC_STATION", "")
	t.Setenv("WEATHER_ECCC_POLL_INTERVAL", "15m")
	if c, err = ConfigFromEnv(); err != nil || c.ECCC.Enabled || c.ECCC.PollInterval != 15*time.Minute {
		t.Fatalf("disabled = %+v, %v", c.ECCC, err)
	}
	t.Setenv("WEATHER_ECCC_ENABLED", "true")
	if _, err = ConfigFromEnv(); err == nil {
		t.Error("enabled without a station accepted")
	}
	t.Setenv("WEATHER_ECCC_STATION", "6144478")
	t.Setenv("WEATHER_ECCC_LOOKBACK", "1h")
	if _, err = ConfigFromEnv(); err == nil {
		t.Error("1h lookback accepted")
	}
	t.Setenv("WEATHER_ECCC_LOOKBACK", "soon")
	if _, err = ConfigFromEnv(); err == nil {
		t.Error("bad duration accepted")
	}
}
