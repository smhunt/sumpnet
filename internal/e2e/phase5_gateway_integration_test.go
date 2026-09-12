//go:build integration

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	alertsv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/alerts/v1"
	queryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/query/v1"
	"github.com/smhunt/sumpnet/internal/alerts"
	"github.com/smhunt/sumpnet/internal/auth/authtest"
	"github.com/smhunt/sumpnet/internal/gateway"
	"github.com/smhunt/sumpnet/internal/platform"
	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
	"github.com/smhunt/sumpnet/internal/testpipeline"
)

// Phase 5 acceptance (prompt_plan.md §12): an owner sees their own home only;
// public views carry segment aggregates only (k >= 3, ADR 0005); the
// neighbourhood stream never carries alerts or per-home identifiers except an
// owner's own alerts. Checked over gRPC and REST against an in-process
// gateway, a real AlertService and an in-test JWKS (never real Clerk).

const dashboardOrigin = "https://dev.ecoworks.ca:3034"

type phase5 struct {
	dsn     string
	st      *store.Store
	q       *sqlcgen.Queries
	is      *authtest.Issuer
	client  queryv1.QueryServiceClient
	restURL string

	home    map[string]uuid.UUID
	dev     map[string]string
	alert   map[string]uuid.UUID // by home name
	storm   []uuid.UUID          // S0 (oldest), S1 (with metrics), S2 (open)
	t0      time.Time
	fcnt    int64
	tokens  map[string]string
	secrets []string // every home id and DevEUI
}

func newPhase5(t *testing.T) *phase5 {
	t.Helper()
	ctx := context.Background()
	p := &phase5{
		dsn: testinfra.StartPostgres(t), home: map[string]uuid.UUID{}, dev: map[string]string{}, alert: map[string]uuid.UUID{},
		t0: time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC),
	}
	if err := store.Migrate(ctx, p.dsn); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, p.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	p.st, p.q = st, st.Queries()
	p.fixtures(t)

	if p.is, err = authtest.NewIssuer(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.is.Close)
	forger, err := authtest.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	p.tokens = map[string]string{
		"user_1":       p.is.MustToken(authtest.TokenOptions{Subject: "user_1", AZP: dashboardOrigin}),
		"user_2":       p.is.MustToken(authtest.TokenOptions{Subject: "user_2", AZP: dashboardOrigin}),
		"expired":      p.is.MustToken(authtest.TokenOptions{Subject: "user_1", AZP: dashboardOrigin, TTL: -time.Minute}),
		"wrong issuer": p.is.MustToken(authtest.TokenOptions{Subject: "user_1", AZP: dashboardOrigin, Issuer: "https://clerk.evil.example"}),
		"wrong azp":    p.is.MustToken(authtest.TokenOptions{Subject: "user_1", AZP: "https://evil.example"}),
		"forged":       p.is.MustToken(authtest.TokenOptions{Subject: "user_1", AZP: dashboardOrigin, Key: forger}),
		"garbage":      "not-a-jwt",
	}
	p.startGateway(t, startAlertService(t, st))
	return p
}

// fixtures: seg-a has 2 linked homes (a1, a2) plus home a3 whose device is
// unlinked; seg-b has 3 linked homes; seg-c none. user_1 owns a1, user_2 b1.
func (p *phase5) fixtures(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	geom := []byte(`{"type":"Polygon","coordinates":[[[-81.43,43.05],[-81.42,43.05],[-81.42,43.06],[-81.43,43.05]]]}`)
	for _, s := range []sqlcgen.SeedSegmentParams{
		{ID: "seg-a", Name: "Segment A", Kind: "standard", Geometry: geom},
		{ID: "seg-b", Name: "Segment B", Kind: "wooded"},
		{ID: "seg-c", Name: "Segment C", Kind: "near_pond"},
	} {
		must(t, p.q.SeedSegment(ctx, s))
	}
	for i, h := range []struct {
		name, segment string
		linked        bool
	}{{"a1", "seg-a", true}, {"a2", "seg-a", true}, {"a3", "seg-a", false}, {"b1", "seg-b", true}, {"b2", "seg-b", true}, {"b3", "seg-b", true}} {
		id := uuid.New()
		p.home[h.name], p.dev[h.name] = id, fmt.Sprintf("70b3d57ed0f0%04x", i+1)
		p.secrets = append(p.secrets, id.String(), p.dev[h.name])
		_, err := p.q.UpsertHome(ctx, sqlcgen.UpsertHomeParams{ID: id, SegmentID: pgtype.Text{String: h.segment, Valid: true}})
		must(t, err)
		link := uuid.NullUUID{UUID: id, Valid: h.linked}
		_, err = p.q.UpsertDevice(ctx, sqlcgen.UpsertDeviceParams{DevEui: p.dev[h.name], Kind: "house", HomeID: link})
		must(t, err)
	}
	for sub, h := range map[string]string{"user_1": "a1", "user_2": "b1"} {
		_, err := p.q.SeedHomeOwner(ctx, sqlcgen.SeedHomeOwnerParams{AuthSubject: sub, HomeID: p.home[h]})
		must(t, err)
	}
	// Heartbeats: seg-b reports 2+4+6 = 12 cycles in the hour ending t0 (4/h per
	// home). The unlinked a3 device reports 50, which must count nowhere.
	p.readings(t, p.t0, map[string]int16{"a1": 5, "a2": 5, "a3": 50, "b1": 2, "b2": 4, "b3": 6})

	for _, s := range []struct {
		start  time.Time
		open   bool
		rainMM float64
	}{{p.t0.AddDate(0, 0, -14), false, 8}, {p.t0.Add(-6 * time.Hour), false, 25}, {p.t0.AddDate(0, 0, 5), true, 3}} {
		var id uuid.UUID
		var end *time.Time
		stat := "open"
		if !s.open {
			e := s.start.Add(4 * time.Hour)
			end, stat = &e, "closed"
		}
		must(t, p.st.Pool().QueryRow(ctx, `INSERT INTO storm_events (started_at, ended_at, total_rain_mm, peak_intensity_mm_h, rain_source, status)
			VALUES ($1, $2, $3, 6, 'gauge', $4) RETURNING id`, s.start, end, s.rainMM, stat).Scan(&id))
		p.storm = append(p.storm, id)
	}
	// S1 metrics: seg-b aggregates to load 600, median lag 15, median recession 150.
	f := func(v float64) *float64 { return &v }
	for _, m := range []struct {
		home          string
		vol           float64
		lag, rec, bfl *float64
	}{
		{"a1", 100, f(30), f(120), f(12)}, {"a2", 200, f(50), nil, nil}, {"a3", 5000, f(1), f(1), nil},
		{"b1", 300, f(10), f(100), nil}, {"b2", 600, f(20), f(200), nil}, {"b3", 900, nil, nil, nil},
	} {
		_, err := p.st.Pool().Exec(ctx, `INSERT INTO home_storm_metrics (storm_id, home_id, lag_min, recession_min, volume_l, cycles, baseflow_cpd)
			VALUES ($1, $2, $3, $4, $5, 7, $6)`, p.storm[1], p.home[m.home], m.lag, m.rec, m.vol, m.bfl)
		must(t, err)
	}
	for _, a := range []struct {
		home   string
		linked bool
		code   alertsv1.AlertCode
	}{{"a1", true, alertsv1.AlertCode_ALERT_CODE_FLOAT_HIGH}, {"b1", true, alertsv1.AlertCode_ALERT_CODE_MAINS_LOST}, {"a3", false, alertsv1.AlertCode_ALERT_CODE_SENSOR_FAULT}} {
		row, err := p.q.InsertAlert(ctx, sqlcgen.InsertAlertParams{
			DeviceID: p.dev[a.home], HomeID: uuid.NullUUID{UUID: p.home[a.home], Valid: a.linked},
			Code: int16(a.code), Severity: 2, RaisedAt: p.t0, Message: "fixture " + a.code.String(),
			Source: "node", TriggerKey: "fixture:" + a.home, NotifyState: "suppressed",
		})
		must(t, err)
		p.alert[a.home] = row.ID
	}
}

func (p *phase5) readings(t *testing.T, ts time.Time, cycles map[string]int16) {
	t.Helper()
	rows := make([]store.Reading, 0, len(cycles))
	for name, c := range cycles {
		p.fcnt++
		rows = append(rows, store.Reading{DeviceID: p.dev[name], TS: ts, FCnt: p.fcnt, LevelMM: 400, TempC: 18, RHPct: 50, BattMV: 3700, CyclesSinceLast: c, MainsOK: true})
	}
	_, err := p.st.InsertReadings(context.Background(), rows)
	must(t, err)
}

func startAlertService(t *testing.T, st *store.Store) string {
	t.Helper()
	srv := grpc.NewServer()
	alertsv1.RegisterAlertServiceServer(srv, alerts.NewServer(st.Queries()))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func (p *phase5) startGateway(t *testing.T, alertsAddr string) {
	t.Helper()
	cfg := gateway.Config{
		DatabaseURL: p.dsn, GRPCAddr: freePort(t), RESTAddr: freePort(t), AlertsAddr: alertsAddr,
		CORSOrigins: []string{dashboardOrigin}, Auth: p.is.Config(dashboardOrigin), JWKSClient: p.is.Client(),
		Watch: gateway.WatchConfig{PollInterval: 200 * time.Millisecond, Debounce: 50 * time.Millisecond, StatusWindow: time.Hour, Buffer: 256, RecentStorms: 5},
	}
	app := platform.NewApp(platform.Config{Service: "api-gateway-test", HTTPAddr: freePort(t), ShutdownTimeout: 5 * time.Second}, quietLog())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- gateway.Run(ctx, app, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("gateway: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("gateway did not stop")
		}
	})
	testpipeline.WaitReady(t, app, done, "api-gateway")
	conn, err := grpc.NewClient(cfg.GRPCAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	must(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	p.client = queryv1.NewQueryServiceClient(conn)
	p.restURL = "http://" + cfg.RESTAddr
}

// ctx returns an outgoing context for caller who ("" = anonymous).
func (p *phase5) ctx(who string) context.Context {
	if who == "" {
		return context.Background()
	}
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+p.tokens[who])
}

func (p *phase5) rest(t *testing.T, method, path, who, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, p.restURL+path, strings.NewReader(body))
	must(t, err)
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+p.tokens[who])
	}
	resp, err := http.DefaultClient.Do(req)
	must(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	must(t, err)
	return resp.StatusCode, b
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, what string, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("%s: got %v, want %v", what, err, code)
	}
}

func TestPhase5Acceptance(t *testing.T) {
	p := newPhase5(t)
	ctx := context.Background()

	t.Run("owner sees own home", func(t *testing.T) {
		resp, err := p.client.GetHome(p.ctx("user_1"), &queryv1.GetHomeRequest{HomeId: p.home["a1"].String()})
		must(t, err)
		h := resp.GetHome()
		if h.GetId() != p.home["a1"].String() || h.GetSegmentId() != "seg-a" || len(h.GetDevices()) != 1 || h.GetDevices()[0].GetDevEui() != p.dev["a1"] {
			t.Fatalf("home = %v", h)
		}
		hh := h.GetHealth()
		if !hh.GetLastSeen().AsTime().Equal(p.t0) || hh.GetBattMv() != 3700 || !hh.GetMainsOk() || hh.GetActiveAlerts() != 1 || hh.GetBaseflowCyclesPerDay() != 12 {
			t.Fatalf("health = %v", hh)
		}
		rs := resp.GetRecentStorms()
		if len(rs) != 1 || rs[0].GetStormId() != p.storm[1].String() || !rs[0].GetLagReached() || rs[0].GetLagMin() != 30 ||
			!rs[0].GetRecessionReached() || rs[0].GetRecessionMin() != 120 || rs[0].GetVolumeL() != 100 {
			t.Fatalf("recent storms = %v", rs)
		}
		code, body := p.rest(t, http.MethodGet, "/v1/homes/"+p.home["a1"].String(), "user_1", "")
		var rr queryv1.GetHomeResponse
		if code != http.StatusOK || protojson.Unmarshal(body, &rr) != nil || !proto.Equal(&rr, resp) {
			t.Fatalf("REST GetHome = %d %s", code, body)
		}
		homes, err := p.client.ListMyHomes(p.ctx("user_1"), &queryv1.ListMyHomesRequest{})
		must(t, err)
		if len(homes.GetHomes()) != 1 || homes.GetHomes()[0].GetId() != p.home["a1"].String() {
			t.Fatalf("ListMyHomes = %v", homes)
		}
	})

	t.Run("another owner's home is not found", func(t *testing.T) {
		for _, c := range []struct{ who, home string }{
			{"user_1", p.home["b1"].String()}, {"user_2", p.home["a1"].String()}, {"user_1", p.home["a2"].String()}, {"user_1", uuid.NewString()},
		} {
			_, err := p.client.GetHome(p.ctx(c.who), &queryv1.GetHomeRequest{HomeId: c.home})
			wantCode(t, c.who+" GetHome "+c.home, err, codes.NotFound)
			if msg := status.Convert(err).Message(); msg != "home not found" {
				t.Fatalf("message %q leaks more than existence", msg)
			}
			if code, _ := p.rest(t, http.MethodGet, "/v1/homes/"+c.home, c.who, ""); code != http.StatusNotFound {
				t.Fatalf("REST %s GetHome = %d, want 404", c.who, code)
			}
		}
		_, err := p.client.ListMyAlerts(p.ctx("user_1"), &queryv1.ListMyAlertsRequest{HomeId: p.home["b1"].String()})
		wantCode(t, "ListMyAlerts for another owner's home", err, codes.NotFound)
	})

	t.Run("anonymous owner calls are unauthenticated", func(t *testing.T) {
		_, err := p.client.GetHome(ctx, &queryv1.GetHomeRequest{HomeId: p.home["a1"].String()})
		wantCode(t, "GetHome", err, codes.Unauthenticated)
		_, err = p.client.ListMyHomes(ctx, &queryv1.ListMyHomesRequest{})
		wantCode(t, "ListMyHomes", err, codes.Unauthenticated)
		_, err = p.client.ListMyAlerts(ctx, &queryv1.ListMyAlertsRequest{})
		wantCode(t, "ListMyAlerts", err, codes.Unauthenticated)
		_, err = p.client.AcknowledgeMyAlert(ctx, &queryv1.AcknowledgeMyAlertRequest{AlertId: p.alert["a1"].String()})
		wantCode(t, "AcknowledgeMyAlert", err, codes.Unauthenticated)
		for _, r := range []struct{ method, path string }{
			{http.MethodGet, "/v1/homes/" + p.home["a1"].String()}, {http.MethodGet, "/v1/me/homes"}, {http.MethodGet, "/v1/me/alerts"},
			{http.MethodPost, "/v1/me/alerts/" + p.alert["a1"].String() + ":acknowledge"},
		} {
			if code, body := p.rest(t, r.method, r.path, "", "{}"); code != http.StatusUnauthorized {
				t.Fatalf("REST anonymous %s %s = %d %s", r.method, r.path, code, body)
			}
		}
	})

	t.Run("forged, expired, wrong-issuer and wrong-azp tokens are rejected", func(t *testing.T) {
		for _, bad := range []string{"expired", "wrong issuer", "wrong azp", "forged", "garbage"} {
			_, err := p.client.GetHome(p.ctx(bad), &queryv1.GetHomeRequest{HomeId: p.home["a1"].String()})
			wantCode(t, bad+" GetHome", err, codes.Unauthenticated)
			_, err = p.client.ListSegments(p.ctx(bad), &queryv1.ListSegmentsRequest{})
			wantCode(t, bad+" ListSegments", err, codes.Unauthenticated)
			stream, err := p.client.WatchNeighbourhood(p.ctx(bad), &queryv1.WatchNeighbourhoodRequest{SendSnapshot: true})
			must(t, err)
			_, err = stream.Recv()
			wantCode(t, bad+" WatchNeighbourhood", err, codes.Unauthenticated)
			for _, path := range []string{"/v1/homes/" + p.home["a1"].String(), "/v1/storm-events", "/v1/neighbourhood:watch?send_snapshot=true"} {
				if code, body := p.rest(t, http.MethodGet, path, bad, ""); code != http.StatusUnauthorized {
					t.Fatalf("REST %s %s = %d %s", bad, path, code, body)
				}
			}
		}
	})

	t.Run("public storm view aggregates segments with at least 3 homes", func(t *testing.T) {
		resp, err := p.client.GetStormEvent(ctx, &queryv1.GetStormEventRequest{StormId: p.storm[1].String()})
		must(t, err)
		want := map[string]*queryv1.SegmentStormMetrics{
			"seg-a": {StormId: p.storm[1].String(), SegmentId: "seg-a", HomesReporting: 2, Suppressed: true},
			"seg-b": {StormId: p.storm[1].String(), SegmentId: "seg-b", HomesReporting: 3, LoadLPerHome: 600, MedianLagMin: 15, MedianRecessionMin: 150},
			"seg-c": {StormId: p.storm[1].String(), SegmentId: "seg-c", Suppressed: true},
		}
		if len(resp.GetSegments()) != len(want) {
			t.Fatalf("segments = %v", resp.GetSegments())
		}
		for _, s := range resp.GetSegments() {
			if !proto.Equal(s, want[s.GetSegmentId()]) {
				t.Errorf("%s = %v, want %v", s.GetSegmentId(), s, want[s.GetSegmentId()])
			}
		}
		code, body := p.rest(t, http.MethodGet, "/v1/storm-events/"+p.storm[1].String(), "", "")
		var rr queryv1.GetStormEventResponse
		if code != http.StatusOK || protojson.Unmarshal(body, &rr) != nil || !proto.Equal(&rr, resp) {
			t.Fatalf("REST GetStormEvent = %d %s", code, body)
		}
		assertNoSecrets(t, "GetStormEvent REST", string(body), p.secrets)
		_, err = p.client.GetStormEvent(ctx, &queryv1.GetStormEventRequest{StormId: uuid.NewString()})
		wantCode(t, "unknown storm", err, codes.NotFound)
	})

	t.Run("storm events page newest first", func(t *testing.T) {
		first, err := p.client.ListStormEvents(ctx, &queryv1.ListStormEventsRequest{PageSize: 2})
		must(t, err)
		if ids(first.GetStormEvents()) != ids2(p.storm[2], p.storm[1]) || first.GetNextPageToken() == "" || first.GetStormEvents()[0].GetEndedAt() != nil {
			t.Fatalf("first page = %v", first)
		}
		second, err := p.client.ListStormEvents(ctx, &queryv1.ListStormEventsRequest{PageSize: 2, PageToken: first.GetNextPageToken()})
		must(t, err)
		if ids(second.GetStormEvents()) != ids2(p.storm[0]) || second.GetNextPageToken() != "" {
			t.Fatalf("second page = %v", second)
		}
		since, err := p.client.ListStormEvents(ctx, &queryv1.ListStormEventsRequest{Since: timestamppb.New(p.t0.Add(-7 * time.Hour))})
		must(t, err)
		if ids(since.GetStormEvents()) != ids2(p.storm[2], p.storm[1]) {
			t.Fatalf("since filter = %v", since)
		}
		code, body := p.rest(t, http.MethodGet, "/v1/storm-events?page_size=2&page_token="+url.QueryEscape(first.GetNextPageToken()), "", "")
		var rr queryv1.ListStormEventsResponse
		if code != http.StatusOK || protojson.Unmarshal(body, &rr) != nil || !proto.Equal(&rr, second) {
			t.Fatalf("REST second page = %d %s", code, body)
		}
		_, err = p.client.ListStormEvents(ctx, &queryv1.ListStormEventsRequest{PageToken: "bogus!"})
		wantCode(t, "bad page token", err, codes.InvalidArgument)
		if code, _ := p.rest(t, http.MethodGet, "/v1/storm-events?page_token=bogus!", "", ""); code != http.StatusBadRequest {
			t.Fatalf("REST bad page token = %d", code)
		}
	})

	t.Run("segment catalogue counts linked homes only", func(t *testing.T) {
		resp, err := p.client.ListSegments(ctx, &queryv1.ListSegmentsRequest{})
		must(t, err)
		got := map[string]*queryv1.Segment{}
		for _, s := range resp.GetSegments() {
			got[s.GetId()] = s
		}
		if got["seg-a"].GetHomeCount() != 2 || got["seg-a"].GetGeometryGeojson() == "" || got["seg-a"].GetKind() != queryv1.SegmentKind_SEGMENT_KIND_STANDARD ||
			got["seg-b"].GetHomeCount() != 3 || got["seg-b"].GetKind() != queryv1.SegmentKind_SEGMENT_KIND_WOODED || got["seg-c"].GetHomeCount() != 0 {
			t.Fatalf("segments = %v", resp)
		}
	})

	t.Run("owner alert list is scoped", func(t *testing.T) {
		for who, home := range map[string]string{"user_1": "a1", "user_2": "b1"} {
			resp, err := p.client.ListMyAlerts(p.ctx(who), &queryv1.ListMyAlertsRequest{})
			must(t, err)
			if len(resp.GetAlerts()) != 1 || resp.GetAlerts()[0].GetId() != p.alert[home].String() {
				t.Fatalf("%s alerts = %v", who, resp)
			}
		}
		code, body := p.rest(t, http.MethodGet, "/v1/me/alerts", "user_1", "")
		if code != http.StatusOK || !strings.Contains(string(body), p.alert["a1"].String()) || strings.Contains(string(body), p.alert["b1"].String()) {
			t.Fatalf("REST ListMyAlerts = %d %s", code, body)
		}
	})

	t.Run("neighbourhood stream privacy", func(t *testing.T) {
		type watcher struct {
			name, who string
			c         *collector
		}
		snap := &queryv1.WatchNeighbourhoodRequest{SendSnapshot: true}
		ws := []*watcher{
			{name: "anonymous gRPC", c: p.watchGRPC(t, "", snap)},
			{name: "anonymous REST", c: p.watchREST(t, "", "send_snapshot=true")},
			{name: "user_1 gRPC", who: "user_1", c: p.watchGRPC(t, "user_1", snap)},
			{name: "user_1 REST", who: "user_1", c: p.watchREST(t, "user_1", "send_snapshot=true")},
			{name: "user_2 gRPC", who: "user_2", c: p.watchGRPC(t, "user_2", snap)},
		}
		owned := map[string]string{"user_1": p.alert["a1"].String(), "user_2": p.alert["b1"].String()}

		for _, w := range ws {
			waitFor(t, w.c, w.name+" snapshot", func(us []*queryv1.NeighbourhoodUpdate) bool {
				n := countKinds(us)
				return n.status >= 3 && n.storm >= 3 && (w.who == "" || n.alert >= 1)
			})
			segB := lastStatus(w.c, "seg-b")
			if segB.GetHomesReporting() != 3 || segB.GetSuppressed() || segB.GetCyclesPerHour() != 4 || segB.GetActiveAlerts() != 1 {
				t.Fatalf("%s seg-b snapshot = %v", w.name, segB)
			}
		}

		// Live: seg-b heartbeats move the neighbourhood clock 15 min on (21
		// cycles in the window = 7/h per home); each owner acknowledges their
		// own alert through the gateway.
		p.readings(t, p.t0.Add(15*time.Minute), map[string]int16{"b1": 3, "b2": 3, "b3": 3, "a3": 90})
		for _, w := range ws {
			waitFor(t, w.c, w.name+" live seg-b status", func([]*queryv1.NeighbourhoodUpdate) bool {
				return lastStatus(w.c, "seg-b").GetCyclesPerHour() == 7
			})
		}
		for who, id := range owned {
			resp, err := p.client.AcknowledgeMyAlert(p.ctx(who), &queryv1.AcknowledgeMyAlertRequest{AlertId: id, Note: "seen"})
			must(t, err)
			if resp.GetAlert().GetAckedAt() == nil {
				t.Fatalf("%s ack = %v", who, resp)
			}
		}
		for _, w := range ws {
			if w.who == "" {
				continue
			}
			waitFor(t, w.c, w.name+" own alert acknowledged", func(us []*queryv1.NeighbourhoodUpdate) bool {
				for _, u := range us {
					if a := u.GetAlert(); a.GetId() == owned[w.who] && a.GetAckedAt() != nil {
						return true
					}
				}
				return false
			})
		}
		time.Sleep(time.Second) // several hub rounds for anything that should not arrive

		for _, w := range ws {
			msgs, raw, err := w.c.snapshot()
			if err != nil {
				t.Fatalf("%s stream ended: %v", w.name, err)
			}
			for i, u := range msgs {
				if s := u.GetSegmentStatus(); s.GetSegmentId() == "seg-a" && (!s.GetSuppressed() || s.GetCyclesPerHour() != 0 || s.GetActiveAlerts() != 0) {
					t.Fatalf("%s: seg-a published below k: %v", w.name, s)
				}
				a := u.GetAlert()
				if a == nil {
					continue
				}
				if w.who == "" {
					t.Fatalf("%s received an alert: %v", w.name, a)
				}
				if a.GetId() != owned[w.who] {
					t.Fatalf("%s received someone else's alert: %v", w.name, a)
				}
				_ = raw[i]
			}
			if w.who == "" {
				for _, line := range raw {
					assertNoSecrets(t, w.name, line, p.secrets)
				}
			}
		}
	})

	t.Run("acknowledge checks ownership", func(t *testing.T) {
		for _, home := range []string{"b1", "a3"} {
			_, err := p.client.AcknowledgeMyAlert(p.ctx("user_1"), &queryv1.AcknowledgeMyAlertRequest{AlertId: p.alert[home].String()})
			wantCode(t, "user_1 acks "+home, err, codes.NotFound)
		}
		if code, _ := p.rest(t, http.MethodPost, "/v1/me/alerts/"+p.alert["b1"].String()+":acknowledge", "user_1", "{}"); code != http.StatusNotFound {
			t.Fatalf("REST cross-owner ack = %d", code)
		}
		var a3Acked bool
		must(t, p.st.Pool().QueryRow(ctx, `SELECT acked_at IS NOT NULL FROM alerts WHERE id = $1`, p.alert["a3"]).Scan(&a3Acked))
		if a3Acked {
			t.Fatal("unlinked alert was acknowledged")
		}
		code, body := p.rest(t, http.MethodPost, "/v1/me/alerts/"+p.alert["a1"].String()+":acknowledge", "user_1", `{"note":"again"}`)
		var rr queryv1.AcknowledgeMyAlertResponse
		if code != http.StatusOK || protojson.Unmarshal(body, &rr) != nil || rr.GetAlert().GetAckedAt() == nil {
			t.Fatalf("REST own ack = %d %s", code, body)
		}
		if code, _ := p.rest(t, http.MethodPost, "/v1/me/alerts/not-a-uuid:acknowledge", "user_1", "{}"); code != http.StatusBadRequest {
			t.Fatalf("REST bad alert id = %d", code)
		}
	})

	t.Run("gateway database access is read-only", func(t *testing.T) {
		pool, err := gateway.OpenReadOnly(ctx, p.dsn)
		must(t, err)
		defer pool.Close()
		_, err = pool.Exec(ctx, `UPDATE alerts SET message = 'tampered'`)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
			t.Fatalf("write through the gateway pool = %v, want read_only_sql_transaction", err)
		}
	})

	t.Run("ops endpoints and CORS on the REST port", func(t *testing.T) {
		if code, _ := p.rest(t, http.MethodGet, "/readyz", "", ""); code != http.StatusOK {
			t.Fatalf("/readyz = %d", code)
		}
		_, metrics := p.rest(t, http.MethodGet, "/metrics", "", "")
		for _, want := range []string{`sumpnet_gateway_auth_total{result="azp"}`, `sumpnet_gateway_auth_total{result="expired"}`, `sumpnet_gateway_auth_total{result="issuer"}`, `sumpnet_gateway_auth_total{result="signature"}`, "sumpnet_gateway_watch_subscribers"} {
			if !strings.Contains(string(metrics), want) {
				t.Errorf("/metrics lacks %s", want)
			}
		}
		req, _ := http.NewRequest(http.MethodOptions, p.restURL+"/v1/me/alerts", nil)
		req.Header.Set("Origin", dashboardOrigin)
		req.Header.Set("Access-Control-Request-Method", "GET")
		req.Header.Set("Access-Control-Request-Headers", "authorization")
		resp, err := http.DefaultClient.Do(req)
		must(t, err)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != dashboardOrigin {
			t.Fatalf("preflight = %d %v", resp.StatusCode, resp.Header)
		}
	})
}

func ids(es []*queryv1.StormEvent) string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.GetId()
	}
	return strings.Join(out, ",")
}

func ids2(us ...uuid.UUID) string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.String()
	}
	return strings.Join(out, ",")
}

func assertNoSecrets(t *testing.T, what, payload string, secrets []string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(payload, s) {
			t.Fatalf("%s leaks per-home identifier %s: %s", what, s, payload)
		}
	}
}

// --- stream collection --------------------------------------------------------

type collector struct {
	mu   sync.Mutex
	msgs []*queryv1.NeighbourhoodUpdate
	raw  []string
	err  error
}

func (c *collector) add(u *queryv1.NeighbourhoodUpdate, raw string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.msgs, c.raw = append(c.msgs, u), append(c.raw, raw)
}

func (c *collector) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func (c *collector) snapshot() ([]*queryv1.NeighbourhoodUpdate, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]*queryv1.NeighbourhoodUpdate(nil), c.msgs...), append([]string(nil), c.raw...), c.err
}

func (p *phase5) watchGRPC(t *testing.T, who string, req *queryv1.WatchNeighbourhoodRequest) *collector {
	t.Helper()
	ctx, cancel := context.WithCancel(p.ctx(who))
	t.Cleanup(cancel)
	stream, err := p.client.WatchNeighbourhood(ctx, req)
	must(t, err)
	c := &collector{}
	go func() {
		for {
			r, err := stream.Recv()
			if err != nil {
				c.fail(err)
				return
			}
			raw, _ := protojson.Marshal(r)
			c.add(r.GetUpdate(), string(raw))
		}
	}()
	return c
}

func (p *phase5) watchREST(t *testing.T, who, query string) *collector {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.restURL+"/v1/neighbourhood:watch?"+query, nil)
	must(t, err)
	if who != "" {
		req.Header.Set("Authorization", "Bearer "+p.tokens[who])
	}
	c := &collector{}
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			c.fail(err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			c.fail(fmt.Errorf("HTTP %d", resp.StatusCode))
			return
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
		for sc.Scan() {
			var env struct {
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(sc.Bytes(), &env); err != nil || env.Error != nil {
				c.fail(fmt.Errorf("stream line %s: %v", sc.Text(), err))
				return
			}
			var r queryv1.WatchNeighbourhoodResponse
			if err := protojson.Unmarshal(env.Result, &r); err != nil {
				c.fail(err)
				return
			}
			c.add(r.GetUpdate(), sc.Text())
		}
		c.fail(fmt.Errorf("stream closed: %w", errors.Join(sc.Err(), io.EOF)))
	}()
	return c
}

type kinds struct{ status, storm, alert int }

func countKinds(us []*queryv1.NeighbourhoodUpdate) kinds {
	var k kinds
	for _, u := range us {
		switch {
		case u.GetSegmentStatus() != nil:
			k.status++
		case u.GetStormEvent() != nil:
			k.storm++
		case u.GetAlert() != nil:
			k.alert++
		}
	}
	return k
}

func lastStatus(c *collector, segment string) *queryv1.SegmentStatus {
	msgs, _, _ := c.snapshot()
	var last *queryv1.SegmentStatus
	for _, u := range msgs {
		if s := u.GetSegmentStatus(); s.GetSegmentId() == segment {
			last = s
		}
	}
	return last
}

func waitFor(t *testing.T, c *collector, what string, done func([]*queryv1.NeighbourhoodUpdate) bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		msgs, _, err := c.snapshot()
		if done(msgs) {
			return
		}
		if err != nil {
			t.Fatalf("%s: stream ended after %d updates: %v", what, len(msgs), err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: timed out after %d updates", what, len(msgs))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
