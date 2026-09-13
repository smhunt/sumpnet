import type { Segment } from './types'

export interface SegmentFeatureProps {
  id: string
  name: string
  fill: string
  hidden: boolean
  selected: boolean
}

export interface SegmentCollection {
  type: 'FeatureCollection'
  features: { type: 'Feature'; id: string; properties: SegmentFeatureProps; geometry: { type: string; coordinates: unknown } }[]
}

// segmentFeatures builds the map source. fillFor returns a colour, or null for
// a street hidden for privacy (drawn hatched). Segments without usable
// geometry are left off the map but stay in the street list.
export function segmentFeatures(segments: Segment[], fillFor: (id: string) => string | null, selectedId: string | null): SegmentCollection {
  const features: SegmentCollection['features'] = []
  for (const s of segments) {
    if (!s.geometryGeojson) continue
    let geometry: { type: string; coordinates: unknown }
    try {
      geometry = JSON.parse(s.geometryGeojson) as { type: string; coordinates: unknown }
    } catch {
      continue
    }
    if (geometry.type !== 'Polygon' && geometry.type !== 'MultiPolygon') continue
    const fill = fillFor(s.id)
    features.push({
      type: 'Feature',
      id: s.id,
      properties: { id: s.id, name: s.name, fill: fill ?? '', hidden: fill === null, selected: s.id === selectedId },
      geometry,
    })
  }
  return { type: 'FeatureCollection', features }
}

// boundsOf returns [[west, south], [east, north]] or null when empty.
export function boundsOf(fc: SegmentCollection): [[number, number], [number, number]] | null {
  let w = Infinity
  let s = Infinity
  let e = -Infinity
  let n = -Infinity
  const visit = (c: unknown): void => {
    if (Array.isArray(c) && typeof c[0] === 'number' && typeof c[1] === 'number') {
      w = Math.min(w, c[0])
      e = Math.max(e, c[0])
      s = Math.min(s, c[1])
      n = Math.max(n, c[1])
    } else if (Array.isArray(c)) {
      c.forEach(visit)
    }
  }
  fc.features.forEach((f) => visit(f.geometry.coordinates))
  return Number.isFinite(w) ? [[w, s], [e, n]] : null
}
