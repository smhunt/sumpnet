package site

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cache layers and file layout: <dir>/<layer dir>/<street slug>.json.
const (
	LayerAddresses = "address_points"
	LayerRoads     = "road_centrelines"
	cacheVersion   = 1
)

// CacheFile is one cached query: the raw ArcGIS response pages for one street
// (address points) or one road (centrelines), with where they came from.
type CacheFile struct {
	Version     int               `json:"version"`
	Layer       string            `json:"layer"`
	Name        string            `json:"name"`
	SourceURL   string            `json:"source_url"`
	Query       map[string]string `json:"query"`
	FetchedAt   time.Time         `json:"fetched_at"`
	Attribution string            `json:"attribution"`
	LicenceNote string            `json:"licence_note"`
	Pages       []json.RawMessage `json:"pages"`
}

// Source serves address and road queries from the cache directory, fetching
// and caching whatever is missing.
type Source struct {
	Client     *Client
	Dir        string // e.g. data/cache/middlesex
	Refresh    bool   // re-query even when a cache file exists
	Offline    bool   // never query; a missing cache file is an error
	AddressURL string // default AddressLayerURL
	RoadURL    string // default RoadLayerURL
	Now        func() time.Time
}

// Addresses returns the cached (or freshly fetched) address points of street.
func (s *Source) Addresses(ctx context.Context, street, munCode string) (*CacheFile, SourceRef, error) {
	u := s.AddressURL
	if u == "" {
		u = AddressLayerURL
	}
	return s.get(ctx, LayerAddresses, u, street, AddressQuery(street, munCode))
}

// Roads returns the cached (or freshly fetched) centreline pieces of road.
func (s *Source) Roads(ctx context.Context, road, munCode string) (*CacheFile, SourceRef, error) {
	u := s.RoadURL
	if u == "" {
		u = RoadLayerURL
	}
	return s.get(ctx, LayerRoads, u, road, RoadQuery(road, munCode))
}

func (s *Source) get(ctx context.Context, layer, layerURL, name string, q Query) (*CacheFile, SourceRef, error) {
	if s.Refresh && s.Offline {
		return nil, SourceRef{}, errors.New("site: refresh and offline are mutually exclusive")
	}
	dir := "addresses"
	if layer == LayerRoads {
		dir = "roads"
	}
	p := filepath.Join(s.Dir, dir, slug(name)+".json")
	sourceURL := strings.TrimSuffix(layerURL, "/") + "/query"
	params := q.Params()
	if !s.Refresh {
		var cf CacheFile
		err := readJSON(p, &cf)
		switch {
		case err == nil && cf.Version == cacheVersion && cf.Layer == layer && cf.SourceURL == sourceURL && maps.Equal(cf.Query, params):
			return &cf, cf.ref(p, true), nil
		case err == nil && s.Offline:
			return nil, SourceRef{}, fmt.Errorf("site: %s was cached with a different source or query; re-import without -offline", p)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return nil, SourceRef{}, err
		}
	}
	if s.Offline {
		return nil, SourceRef{}, fmt.Errorf("site: %s %q is not cached (%s) and -offline is set", layer, name, p)
	}
	if s.Client == nil {
		return nil, SourceRef{}, errors.New("site: no ArcGIS client")
	}
	pages, err := s.Client.Fetch(ctx, layerURL, q)
	if err != nil {
		return nil, SourceRef{}, fmt.Errorf("site: fetch %s %q: %w", layer, name, err)
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	cf := &CacheFile{
		Version: cacheVersion, Layer: layer, Name: name, SourceURL: sourceURL, Query: params,
		FetchedAt: now().UTC().Truncate(time.Second), Attribution: Attribution, LicenceNote: LicenceNote, Pages: pages,
	}
	if err := writeJSON(p, cf, false); err != nil {
		return nil, SourceRef{}, err
	}
	return cf, cf.ref(p, false), nil
}

// SourceRef records where a street's data came from in the snapshot.
type SourceRef struct {
	Layer     string            `json:"layer"`
	URL       string            `json:"url"`
	Query     map[string]string `json:"query"`
	FetchedAt time.Time         `json:"fetched_at"`
	CacheFile string            `json:"cache_file"`
	Pages     int               `json:"pages"`
	FromCache bool              `json:"-"`
}

func (cf *CacheFile) ref(path string, fromCache bool) SourceRef {
	return SourceRef{Layer: cf.Layer, URL: cf.SourceURL, Query: cf.Query, FetchedAt: cf.FetchedAt, CacheFile: path, Pages: len(cf.Pages), FromCache: fromCache}
}

// LoadOrCreateSalt reads the hex identity salt at path, creating a random
// 32-byte one if the file does not exist. The salt keys the home identifiers
// (ADR 0008): keep it with the cache and never commit it.
func LoadOrCreateSalt(path string) (salt []byte, created bool, err error) {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied cache path
	if err == nil {
		salt, err = hex.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(salt) < 16 {
			return nil, false, fmt.Errorf("site: %s: want at least 16 hex-encoded bytes", path)
		}
		return salt, false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, false, fmt.Errorf("site: salt: %w", err)
	}
	salt = make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return nil, false, fmt.Errorf("site: salt: %w", err)
	}
	if err := writeFileAtomic(path, []byte(hex.EncodeToString(salt)+"\n")); err != nil {
		return nil, false, err
	}
	return salt, true, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied cache path
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("site: %s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any, indent bool) error {
	var b []byte
	var err error
	if indent {
		b, err = json.MarshalIndent(v, "", "  ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		return fmt.Errorf("site: encode %s: %w", path, err)
	}
	return writeFileAtomic(path, append(b, '\n'))
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("site: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("site: %w", err)
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("site: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("site: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("site: %w", err)
	}
	return nil
}
