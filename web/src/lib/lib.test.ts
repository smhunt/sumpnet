import { describe, expect, it } from 'vitest'
import { reconnectDelay } from './api'
import { bucket, heatColor, niceMax, WATER_SCALE } from './heat'
import { boundsOf, segmentFeatures } from './mapdata'
import { NDJSONDecoder, parseLine } from './ndjson'
import {
  alertTitle,
  applyUpdate,
  emptyNeighbourhood,
  hiddenReason,
  liveView,
  MIN_HOMES,
  sortAlerts,
  stormView,
} from './neighbourhood'
import { chronological, clampIndex, gaugeMarks } from './storms'
import type { Alert, NeighbourhoodUpdate, SegmentStatus, StormEvent } from './types'

const status = (id: string, homes: number, suppressed: boolean, cph = 0, alerts = 0): SegmentStatus => ({
  segmentId: id,
  homesReporting: homes,
  suppressed,
  cyclesPerHour: cph,
  activeAlerts: alerts,
})

const storm = (id: string, startedAt: string, mm: number, endedAt: string | null = null): StormEvent => ({
  id,
  startedAt,
  endedAt,
  totalRainMm: mm,
  peakIntensityMmH: 4,
})

const alert = (id: string, extra: Partial<Alert> = {}): Alert => ({
  id,
  homeId: 'h',
  segmentId: 'seg-01',
  code: 'ALERT_CODE_FLOAT_HIGH',
  severity: 'ALERT_SEVERITY_WARNING',
  raisedAt: '2026-04-15T12:00:00Z',
  message: '',
  deviceId: 'd',
  ...extra,
})

describe('ndjson', () => {
  it('reassembles lines split across chunks', () => {
    const d = new NDJSONDecoder<{ update: NeighbourhoodUpdate }>()
    expect(d.push('{"result":{"update":{"ts":"2026-04-15T12:00:00Z","segmentStatus":{"segmentId":"seg-01"')).toEqual([])
    const events = d.push(',"homesReporting":3}}}}\n{"result":{"update":{"stormEvent":{"id":"s"}}}}\n')
    expect(events).toHaveLength(2)
    expect(events[0]).toMatchObject({ kind: 'result', value: { update: { segmentStatus: { segmentId: 'seg-01' } } } })
    expect(d.flush()).toEqual([])
  })

  it('surfaces an error chunk and a trailing line without newline', () => {
    const d = new NDJSONDecoder()
    expect(d.push('{"error":{"code":16,"message":"invalid or expired token"}}')).toEqual([])
    expect(d.flush()).toEqual([{ kind: 'error', code: 16, message: 'invalid or expired token' }])
  })

  it('ignores blank lines and rejects foreign JSON', () => {
    expect(parseLine('   ')).toBeNull()
    expect(() => parseLine('{"hello":1}')).toThrow()
  })
})

describe('privacy display state', () => {
  it('hides any segment below the threshold even if the flag is missing', () => {
    expect(liveView(status('a', 2, false, 9))).toEqual({ state: 'hidden', homesReporting: 2 })
    expect(liveView(status('a', 5, true, 9))).toEqual({ state: 'hidden', homesReporting: 5 })
    expect(liveView(status('a', MIN_HOMES, false, 4.5))).toEqual({ state: 'visible', homesReporting: 3, value: 4.5 })
    expect(liveView(undefined)).toEqual({ state: 'no-data' })
  })

  it('applies the same rule to storm aggregates', () => {
    const m = { stormId: 's', segmentId: 'a', homesReporting: 2, suppressed: false, loadLPerHome: 300, medianLagMin: 10, medianRecessionMin: 20 }
    expect(stormView(m).state).toBe('hidden')
    expect(stormView({ ...m, homesReporting: 4, suppressed: false })).toEqual({ state: 'visible', homesReporting: 4, value: 300 })
  })

  it('explains suppression in plain words, never as zero', () => {
    expect(hiddenReason(0)).toMatch(/No homes/)
    expect(hiddenReason(2)).toBe('Hidden for privacy: 2 of 3 homes report here. It appears once 1 more neighbour joins.')
    expect(hiddenReason(1)).toMatch(/2 more neighbours join/)
  })
})

describe('neighbourhood state', () => {
  it('upserts statuses and storms and tracks the as-of time', () => {
    let s = applyUpdate(emptyNeighbourhood, { ts: '2026-04-15T12:00:00Z', segmentStatus: status('seg-01', 3, false, 4) })
    s = applyUpdate(s, { ts: '2026-04-15T12:15:00Z', segmentStatus: status('seg-01', 3, false, 7) })
    s = applyUpdate(s, { stormEvent: storm('s1', '2026-04-15T06:00:00Z', 25) })
    expect(s.statuses['seg-01']?.cyclesPerHour).toBe(7)
    expect(s.asOf).toBe('2026-04-15T12:15:00Z')
    expect(Object.keys(s.storms)).toEqual(['s1'])
    expect(emptyNeighbourhood.statuses).toEqual({})
  })

  it('drops resolved alerts', () => {
    let s = applyUpdate(emptyNeighbourhood, { alert: alert('a1') })
    expect(Object.keys(s.alerts)).toEqual(['a1'])
    s = applyUpdate(s, { alert: alert('a1', { resolvedAt: '2026-04-15T13:00:00Z' }) })
    expect(s.alerts).toEqual({})
  })

  it('orders alerts unacknowledged, severe, newest first', () => {
    const sorted = sortAlerts([
      alert('acked', { severity: 'ALERT_SEVERITY_CRITICAL', ackedAt: '2026-04-15T12:30:00Z' }),
      alert('warn-old', { raisedAt: '2026-04-15T10:00:00Z' }),
      alert('crit', { severity: 'ALERT_SEVERITY_CRITICAL' }),
      alert('warn-new', { raisedAt: '2026-04-15T13:00:00Z' }),
    ])
    expect(sorted.map((a) => a.id)).toEqual(['crit', 'warn-new', 'warn-old', 'acked'])
    expect(alertTitle('ALERT_CODE_MAINS_LOST')).toBe('Power lost at the pump')
    expect(alertTitle('ALERT_CODE_SOMETHING_NEW')).toBe('something new')
  })
})

describe('heat scale', () => {
  it('buckets values and rounds the top', () => {
    expect(niceMax([3.2])).toBe(5)
    expect(niceMax([0])).toBe(1)
    expect(niceMax([12, 180])).toBe(200)
    expect(bucket(0, 10)).toBe(0)
    expect(bucket(10, 10)).toBe(WATER_SCALE.length - 1)
    expect(bucket(99, 10)).toBe(WATER_SCALE.length - 1)
    expect(heatColor(Number.NaN, 10)).toBe(WATER_SCALE[0])
  })
})

describe('storm replay', () => {
  it('works with no storms', () => {
    expect(chronological([])).toEqual([])
    expect(clampIndex(3, 0)).toBe(-1)
    expect(gaugeMarks([])).toEqual([])
  })

  it('orders oldest first and clamps the slider', () => {
    const list = chronological([storm('b', '2026-04-20T00:00:00Z', 5), storm('a', '2026-04-01T00:00:00Z', 50, '2026-04-01T08:00:00Z')])
    expect(list.map((s) => s.id)).toEqual(['a', 'b'])
    expect(clampIndex(-4, 2)).toBe(0)
    expect(clampIndex(7, 2)).toBe(1)
    expect(clampIndex(Number.NaN, 2)).toBe(1)
    const marks = gaugeMarks(list)
    expect(marks[0]).toEqual({ id: 'a', position: 0, weight: 1 })
    expect(marks[1]?.position).toBe(1)
    expect(marks[1]?.weight).toBeCloseTo(0.15)
    expect(gaugeMarks([list[0]!])[0]?.position).toBe(0.5)
  })
})

describe('reconnect backoff', () => {
  it('grows and caps', () => {
    expect(reconnectDelay(0, () => 0)).toBe(500)
    expect(reconnectDelay(0, () => 1)).toBe(1000)
    expect(reconnectDelay(3, () => 1)).toBe(8000)
    expect(reconnectDelay(20, () => 1)).toBe(30000)
  })
})

describe('map data', () => {
  const seg = (id: string, geometryGeojson: string) => ({ id, name: id, geometryGeojson, homeCount: 3, kind: 'SEGMENT_KIND_STANDARD' as const })
  const poly = '{"type":"Polygon","coordinates":[[[-81.43,43.05],[-81.42,43.05],[-81.42,43.06],[-81.43,43.05]]]}'

  it('marks hidden streets and skips missing or invalid geometry', () => {
    const fc = segmentFeatures(
      [seg('a', poly), seg('b', poly), seg('c', ''), seg('d', '{nope'), seg('e', '{"type":"Point","coordinates":[1,2]}')],
      (id) => (id === 'a' ? '#5FA3B8' : null),
      'b',
    )
    expect(fc.features.map((f) => f.id)).toEqual(['a', 'b'])
    expect(fc.features[0]?.properties).toMatchObject({ fill: '#5FA3B8', hidden: false, selected: false })
    expect(fc.features[1]?.properties).toMatchObject({ fill: '', hidden: true, selected: true })
    expect(boundsOf(fc)).toEqual([[-81.43, 43.05], [-81.42, 43.06]])
    expect(boundsOf({ type: 'FeatureCollection', features: [] })).toBeNull()
  })
})
