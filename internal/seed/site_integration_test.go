//go:build integration

package seed

import (
	"context"
	"strings"
	"testing"

	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/testinfra"
)

func TestApplySite(t *testing.T) {
	ctx := context.Background()
	dsn := testinfra.StartPostgres(t)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	b := testSite()
	o := Options{Seed: 42, Site: b, OwnerSubject: "user_site", OwnerAddress: " 3 oak lane "}
	want, _ := b.Locate("3 OAK LANE")
	for range 2 {
		res := applyTx(t, st, o)
		if res.Site != "testville" || res.Segments != 2 || res.SegmentsWithGeo != 2 || res.Homes != 6 || res.Devices != 6 ||
			res.OwnerHomeID != b.Homes[want].HomeID || res.OwnerSegment != "oak-lane-1" {
			t.Fatalf("result = %+v", res)
		}
	}
	var segs, homes, linked, owners int
	var ownerHome string
	err = st.Pool().QueryRow(ctx, `SELECT
		(SELECT count(*) FROM segments WHERE geometry IS NOT NULL AND kind = 'wooded'),
		(SELECT count(*) FROM homes), (SELECT count(*) FROM devices WHERE home_id IS NOT NULL AND dev_eui LIKE '5e%'),
		(SELECT count(*) FROM home_owners WHERE auth_subject = 'user_site'),
		(SELECT home_id::text FROM home_owners WHERE auth_subject = 'user_site')`).Scan(&segs, &homes, &linked, &owners, &ownerHome)
	if err != nil {
		t.Fatal(err)
	}
	if segs != 2 || homes != 6 || linked != 6 || owners != 1 || ownerHome != b.Homes[want].HomeID {
		t.Fatalf("db: segments %d, homes %d, linked %d, owners %d (%s)", segs, homes, linked, owners, ownerHome)
	}
	// No address text anywhere in what the seed wrote.
	var dump string
	err = st.Pool().QueryRow(ctx, `SELECT concat(
		(SELECT string_agg(row_to_json(s)::text, '') FROM segments s),
		(SELECT string_agg(row_to_json(h)::text, '') FROM homes h),
		(SELECT string_agg(row_to_json(d)::text, '') FROM devices d))`).Scan(&dump)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range b.Homes {
		if strings.Contains(strings.ToUpper(dump), h.Address) {
			t.Fatalf("database contains a site address")
		}
	}
}
