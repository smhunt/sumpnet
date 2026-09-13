import type { Alert, NeighbourhoodUpdate, SegmentStatus, SegmentStormMetrics, StormEvent } from './types'

// MIN_HOMES mirrors internal/privacy.MinHomes (ADR 0005). The gateway already
// suppresses smaller aggregates; the dashboard hides them again rather than
// trusting a flag, and never draws a hidden street as "zero".
export const MIN_HOMES = 3

export interface NeighbourhoodState {
  statuses: Record<string, SegmentStatus>
  storms: Record<string, StormEvent>
  alerts: Record<string, Alert> // only ever the signed-in owner's own
  asOf: string | null // event time the live statuses describe
}

export const emptyNeighbourhood: NeighbourhoodState = { statuses: {}, storms: {}, alerts: {}, asOf: null }

export function applyUpdate(state: NeighbourhoodState, u: NeighbourhoodUpdate): NeighbourhoodState {
  if (u.segmentStatus) {
    return {
      ...state,
      statuses: { ...state.statuses, [u.segmentStatus.segmentId]: u.segmentStatus },
      asOf: u.ts ?? state.asOf,
    }
  }
  if (u.stormEvent) {
    return { ...state, storms: { ...state.storms, [u.stormEvent.id]: u.stormEvent } }
  }
  if (u.alert) {
    const alerts = { ...state.alerts }
    if (u.alert.resolvedAt) delete alerts[u.alert.id]
    else alerts[u.alert.id] = u.alert
    return { ...state, alerts }
  }
  return state
}

export type AggregateView =
  | { state: 'no-data' }
  | { state: 'hidden'; homesReporting: number }
  | { state: 'visible'; homesReporting: number; value: number }

function view(homesReporting: number, suppressed: boolean, value: number): AggregateView {
  if (suppressed || homesReporting < MIN_HOMES) return { state: 'hidden', homesReporting }
  return { state: 'visible', homesReporting, value }
}

export function liveView(s: SegmentStatus | undefined): AggregateView {
  return s ? view(s.homesReporting, s.suppressed, s.cyclesPerHour) : { state: 'no-data' }
}

export function stormView(m: SegmentStormMetrics | undefined): AggregateView {
  return m ? view(m.homesReporting, m.suppressed, m.loadLPerHome) : { state: 'no-data' }
}

export function hiddenReason(homesReporting: number): string {
  if (homesReporting <= 0) return 'No homes on this street report yet.'
  const more = MIN_HOMES - homesReporting
  return `Hidden for privacy: ${homesReporting} of ${MIN_HOMES} homes report here. It appears once ${more} more ${more === 1 ? 'neighbour joins' : 'neighbours join'}.`
}

const severityRank: Record<string, number> = {
  ALERT_SEVERITY_CRITICAL: 3,
  ALERT_SEVERITY_WARNING: 2,
  ALERT_SEVERITY_INFO: 1,
}

// sortAlerts: unacknowledged first, then most severe, then newest.
export function sortAlerts(alerts: Alert[]): Alert[] {
  return [...alerts].sort(
    (a, b) =>
      Number(Boolean(a.ackedAt)) - Number(Boolean(b.ackedAt)) ||
      (severityRank[b.severity] ?? 0) - (severityRank[a.severity] ?? 0) ||
      b.raisedAt.localeCompare(a.raisedAt),
  )
}

const codeNames: Record<string, string> = {
  ALERT_CODE_FLOAT_HIGH: 'High water in the pit',
  ALERT_CODE_MAINS_LOST: 'Power lost at the pump',
  ALERT_CODE_DRY_RUN: 'Pump running without moving water',
  ALERT_CODE_CONTINUOUS_RUN: 'Pump running non-stop',
  ALERT_CODE_SENSOR_FAULT: 'Sensor fault',
  ALERT_CODE_SHORT_CYCLING: 'Pump short-cycling',
  ALERT_CODE_OUTAGE_RISK: 'Outage risk: power off and water rising',
  ALERT_CODE_LOW_BATTERY: 'Sensor battery low',
  ALERT_CODE_OFFLINE: 'Sensor offline',
}

export function alertTitle(code: string): string {
  return codeNames[code] ?? code.replace(/^ALERT_CODE_/, '').toLowerCase().replace(/_/g, ' ')
}
