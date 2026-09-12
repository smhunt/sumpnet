import 'maplibre-gl/dist/maplibre-gl.css'
import { AttributionControl, Map as MapLibre, NavigationControl, type GeoJSONSource, type StyleSpecification } from 'maplibre-gl'
import { useEffect, useMemo, useRef, useState } from 'react'
import { boundsOf, segmentFeatures } from '../lib/mapdata'
import type { Segment } from '../lib/types'

const TILE_URL = import.meta.env.VITE_TILE_URL || 'https://tile.openstreetmap.org/{z}/{x}/{y}.png'
const TILE_ATTRIBUTION =
  import.meta.env.VITE_TILE_ATTRIBUTION || '© <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
// Nominal centre of the illustrative Timberwalk outlines (Ilderton, ON).
const TIMBERWALK: [number, number] = [-81.4236, 43.056]

const style: StyleSpecification = {
  version: 8,
  sources: { base: { type: 'raster', tiles: [TILE_URL], tileSize: 256, maxzoom: 19, attribution: TILE_ATTRIBUTION } },
  layers: [{ id: 'base', type: 'raster', source: 'base', paint: { 'raster-saturation': -0.7, 'raster-opacity': 0.9 } }],
}

// hatch is the privacy texture: graphite diagonals on transparency.
function hatch(): { width: number; height: number; data: Uint8Array } {
  const size = 8
  const data = new Uint8Array(size * size * 4)
  for (let y = 0; y < size; y++) {
    for (let x = 0; x < size; x++) {
      const on = (x + y) % size < 2
      const i = (y * size + x) * 4
      data.set(on ? [0x8a, 0x94, 0x9b, 230] : [0xe9, 0xee, 0xf0, 90], i)
    }
  }
  return { width: size, height: size, data }
}

interface Props {
  segments: Segment[]
  fillFor: (id: string) => string | null
  selectedId: string | null
  onSelect: (id: string | null) => void
}

export function MapView({ segments, fillFor, selectedId, onSelect }: Props) {
  const container = useRef<HTMLDivElement>(null)
  const map = useRef<MapLibre | null>(null)
  const fitted = useRef(false)
  const onSelectRef = useRef(onSelect)
  const [ready, setReady] = useState(false)

  useEffect(() => {
    onSelectRef.current = onSelect
  }, [onSelect])

  useEffect(() => {
    if (!container.current) return
    const m = new MapLibre({ container: container.current, style, center: TIMBERWALK, zoom: 15, attributionControl: false })
    m.addControl(new NavigationControl({ showCompass: false }), 'top-left')
    m.addControl(new AttributionControl({ compact: true }), 'bottom-right')
    m.on('load', () => {
      m.addImage('privacy-hatch', hatch())
      m.addSource('segments', { type: 'geojson', data: { type: 'FeatureCollection', features: [] } })
      m.addLayer({ id: 'segment-fill', type: 'fill', source: 'segments', filter: ['!', ['get', 'hidden']], paint: { 'fill-color': ['get', 'fill'], 'fill-opacity': 0.8 } })
      m.addLayer({ id: 'segment-hidden', type: 'fill', source: 'segments', filter: ['get', 'hidden'], paint: { 'fill-pattern': 'privacy-hatch' } })
      m.addLayer({
        id: 'segment-line',
        type: 'line',
        source: 'segments',
        paint: { 'line-color': ['case', ['get', 'selected'], '#1F2A33', '#52606B'], 'line-width': ['case', ['get', 'selected'], 3, 1] },
      })
      m.on('click', (e) => {
        const hit = m.queryRenderedFeatures(e.point, { layers: ['segment-fill', 'segment-hidden'] })[0]
        onSelectRef.current((hit?.properties?.id as string | undefined) ?? null)
      })
      for (const layer of ['segment-fill', 'segment-hidden']) {
        m.on('mouseenter', layer, () => (m.getCanvas().style.cursor = 'pointer'))
        m.on('mouseleave', layer, () => (m.getCanvas().style.cursor = ''))
      }
      setReady(true)
    })
    map.current = m
    return () => {
      m.remove()
      map.current = null
      fitted.current = false
    }
  }, [])

  const data = useMemo(() => segmentFeatures(segments, fillFor, selectedId), [segments, fillFor, selectedId])

  useEffect(() => {
    const m = map.current
    if (!ready || !m) return
    void (m.getSource('segments') as GeoJSONSource | undefined)?.setData(data as unknown as Parameters<GeoJSONSource['setData']>[0])
    const b = boundsOf(data)
    if (b && !fitted.current) {
      m.fitBounds(b, { padding: { top: 60, bottom: 60, left: 60, right: 200 }, duration: 0, maxZoom: 17 })
      fitted.current = true
    }
  }, [ready, data])

  return <div ref={container} className="map" role="region" aria-label="Map of Timberwalk streets" />
}
