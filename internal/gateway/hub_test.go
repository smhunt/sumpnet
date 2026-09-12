package gateway

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	queryv1 "github.com/smhunt/sumpnet/gen/go/sumpnet/query/v1"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
)

func testHub(buffer int) *Hub {
	cfg := DefaultWatchConfig()
	cfg.Buffer = buffer
	return NewHub(nil, "", cfg, NewMetrics(prometheus.NewRegistry()), slog.New(slog.DiscardHandler))
}

func drain(s *subscriber) []*queryv1.NeighbourhoodUpdate {
	var out []*queryv1.NeighbourhoodUpdate
	for {
		select {
		case u := <-s.ch:
			out = append(out, u)
		default:
			return out
		}
	}
}

type counts struct{ status, storm, alert int }

func count(us []*queryv1.NeighbourhoodUpdate) counts {
	var c counts
	for _, u := range us {
		switch {
		case u.GetSegmentStatus() != nil:
			c.status++
		case u.GetStormEvent() != nil:
			c.storm++
		case u.GetAlert() != nil:
			c.alert++
		}
	}
	return c
}

func statuses(cph float64) (map[string]*queryv1.SegmentStatus, []string) {
	return map[string]*queryv1.SegmentStatus{
		"seg-a": {SegmentId: "seg-a", HomesReporting: 2, Suppressed: true},
		"seg-b": {SegmentId: "seg-b", HomesReporting: 3, CyclesPerHour: cph},
	}, []string{"seg-a", "seg-b"}
}

func TestPublishRouting(t *testing.T) {
	h := testHub(64)
	anon := newSubscriber("", nil, 64)
	owner1 := newSubscriber("user_1", nil, 64)
	owner1SegB := newSubscriber("user_1", []string{"seg-b"}, 64)
	owner2 := newSubscriber("user_2", nil, 64)
	for _, s := range []*subscriber{anon, owner1, owner1SegB, owner2} {
		h.subs[s] = struct{}{}
	}
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	alertA := alertUpdate(sqlcgen.Alert{ID: uuid.New(), HomeID: uuid.NullUUID{UUID: hid(1), Valid: true}, UpdatedAt: now},
		pgtype.Text{String: "seg-a", Valid: true}, []string{"user_1"})
	unowned := alertUpdate(sqlcgen.Alert{ID: uuid.New(), HomeID: uuid.NullUUID{UUID: hid(9), Valid: true}, UpdatedAt: now},
		pgtype.Text{String: "seg-b", Valid: true}, nil)
	storm := stormUpdate(sqlcgen.StormEvent{ID: uuid.New(), StartedAt: now, UpdatedAt: now})

	st, order := statuses(4)
	h.publish(now, st, order, []routed{storm, alertA, unowned})
	tests := []struct {
		name string
		sub  *subscriber
		want counts
	}{
		{"anonymous: statuses and storms, never alerts", anon, counts{2, 1, 0}},
		{"owner of the alert's home", owner1, counts{2, 1, 1}},
		{"owner filtered to another segment", owner1SegB, counts{1, 1, 0}},
		{"another owner", owner2, counts{2, 1, 0}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := drain(tc.sub)
			if c := count(got); c != tc.want {
				t.Fatalf("counts = %+v, want %+v", c, tc.want)
			}
			for _, u := range got {
				if a := u.GetAlert(); a != nil && a.GetHomeId() != hid(1).String() {
					t.Fatalf("received someone else's alert %v", a)
				}
			}
		})
	}

	// Unchanged statuses are not re-sent; a changed one is, alone.
	h.publish(now.Add(time.Minute), st, order, nil)
	if n := len(drain(anon)); n != 0 {
		t.Fatalf("unchanged round delivered %d updates", n)
	}
	st2, _ := statuses(6)
	h.publish(now.Add(2*time.Minute), st2, order, nil)
	got := drain(anon)
	if len(got) != 1 || got[0].GetSegmentStatus().GetSegmentId() != "seg-b" || !got[0].GetTs().AsTime().Equal(now.Add(2*time.Minute)) {
		t.Fatalf("changed round delivered %v", got)
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	h := testHub(16)
	slow := newSubscriber("", nil, 16)
	h.subs[slow] = struct{}{}
	_, order := statuses(0)
	for i := range 20 {
		st, _ := statuses(float64(i + 1))
		h.publish(time.Unix(int64(i), 0), st, order, nil)
	}
	select {
	case <-slow.gone:
	default:
		t.Fatal("subscriber with a full queue was not dropped")
	}
	if _, ok := h.subs[slow]; ok {
		t.Fatal("dropped subscriber still registered")
	}
}

func TestServeLiveUpdatesAndShutdown(t *testing.T) {
	h := testHub(16)
	h.readyOnce.Do(func() { close(h.ready) })
	got := make(chan *queryv1.WatchNeighbourhoodResponse, 16)
	errc := make(chan error, 1)
	go func() {
		errc <- h.Serve(context.Background(), &queryv1.WatchNeighbourhoodRequest{}, "", func(r *queryv1.WatchNeighbourhoodResponse) error {
			got <- r
			return nil
		})
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		n := len(h.subs)
		h.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("subscriber never registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	st, order := statuses(3)
	h.publish(time.Unix(100, 0), st, order, nil)
	for range 2 {
		select {
		case r := <-got:
			if r.GetUpdate().GetSegmentStatus() == nil {
				t.Fatalf("unexpected update %v", r)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no live update")
		}
	}
	close(h.done)
	select {
	case err := <-errc:
		if status.Code(err) != codes.Unavailable {
			t.Fatalf("Serve after shutdown = %v, want UNAVAILABLE", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return on shutdown")
	}
}

func TestChangeTracker(t *testing.T) {
	base := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	c := newChangeTracker(base)
	a, b := uuid.New(), uuid.New()
	if !c.isNew(a, base.Add(time.Second)) {
		t.Fatal("unseen row must be new")
	}
	c.record([]version{{a, base.Add(time.Second)}, {b, base.Add(-30 * time.Second)}})
	if c.isNew(a, base.Add(time.Second)) || !c.isNew(a, base.Add(2*time.Second)) {
		t.Fatal("same version must not repeat; a newer version must be new")
	}
	if !c.since().Equal(base.Add(time.Second - changeOverlap)) {
		t.Fatalf("since = %v", c.since())
	}
	c.record([]version{{a, base.Add(5 * time.Minute)}})
	if _, ok := c.seen[b]; ok {
		t.Fatal("versions older than the overlap must be pruned")
	}
}
