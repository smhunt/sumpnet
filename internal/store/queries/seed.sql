-- Demo seed (cmd/seed): operator tooling only, never used by a service.
-- Homes and devices go through UpsertHome / UpsertDevice.

-- name: SeedSegment :exec
INSERT INTO segments (id, name, kind, geometry) VALUES (@id, @name, @kind, @geometry)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, kind = EXCLUDED.kind, geometry = EXCLUDED.geometry;

-- name: SeedHomeOwner :execrows
INSERT INTO home_owners (auth_subject, home_id) VALUES (@auth_subject, @home_id)
ON CONFLICT DO NOTHING;
