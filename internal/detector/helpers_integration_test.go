//go:build integration

package detector

import (
	"database/sql"

	"github.com/jackc/pgx/v5/pgtype"
)

func pgText(s string) pgtype.Text         { return pgtype.Text{String: s, Valid: true} }
func nullFloat(f float64) sql.NullFloat64 { return sql.NullFloat64{Float64: f, Valid: true} }
