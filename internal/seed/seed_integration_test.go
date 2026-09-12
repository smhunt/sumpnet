//go:build integration

package seed

import (
	"context"
	"testing"

	"github.com/smhunt/sumpnet/internal/store"
	"github.com/smhunt/sumpnet/internal/store/sqlcgen"
	"github.com/smhunt/sumpnet/internal/testinfra"
)

func TestApplyIdempotent(t *testing.T) {
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
	o := DefaultOptions()
	o.OwnerSubject, o.OwnerHome = "user_demo", 3
	for range 2 {
		res := applyTx(t, st, o)
		if res.Segments != 8 || res.SegmentsWithGeo != 8 || res.Homes != 60 || res.Devices != 60 || res.OwnerHomeID == "" {
			t.Fatalf("result = %+v", res)
		}
	}
	var segs, geo, homes, linked, owners int
	err = st.Pool().QueryRow(ctx, `SELECT
		(SELECT count(*) FROM segments), (SELECT count(*) FROM segments WHERE geometry IS NOT NULL),
		(SELECT count(*) FROM homes), (SELECT count(*) FROM devices WHERE home_id IS NOT NULL),
		(SELECT count(*) FROM home_owners WHERE auth_subject = 'user_demo')`).Scan(&segs, &geo, &homes, &linked, &owners)
	if err != nil {
		t.Fatal(err)
	}
	if segs != 8 || geo != 8 || homes != 60 || linked != 60 || owners != 1 {
		t.Fatalf("db: segments %d (geo %d), homes %d, linked devices %d, owner links %d", segs, geo, homes, linked, owners)
	}
}

func applyTx(t *testing.T, st *store.Store, o Options) Result {
	t.Helper()
	ctx := context.Background()
	tx, err := st.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Apply(ctx, sqlcgen.New(tx), o)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return res
}
