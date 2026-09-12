// JSON shapes of query.v1 / alerts.v1 as the api-gateway emits them
// (protojson: camelCase, enums as names, unset messages as null).

export type SegmentKind =
  | 'SEGMENT_KIND_UNSPECIFIED'
  | 'SEGMENT_KIND_STANDARD'
  | 'SEGMENT_KIND_WOODED'
  | 'SEGMENT_KIND_NEAR_POND'
  | 'SEGMENT_KIND_HIGH_GROUND'

export interface Segment {
  id: string
  name: string
  geometryGeojson: string
  homeCount: number
  kind: SegmentKind
}

export interface SegmentStatus {
  segmentId: string
  homesReporting: number
  suppressed: boolean
  cyclesPerHour: number
  activeAlerts: number
}

export interface StormEvent {
  id: string
  startedAt: string
  endedAt?: string | null
  totalRainMm: number
  peakIntensityMmH: number
}

export interface SegmentStormMetrics {
  stormId: string
  segmentId: string
  homesReporting: number
  suppressed: boolean
  loadLPerHome: number
  medianLagMin: number
  medianRecessionMin: number
}

export type AlertSeverity =
  | 'ALERT_SEVERITY_UNSPECIFIED'
  | 'ALERT_SEVERITY_INFO'
  | 'ALERT_SEVERITY_WARNING'
  | 'ALERT_SEVERITY_CRITICAL'

export interface Alert {
  id: string
  homeId: string
  segmentId: string
  code: string
  severity: AlertSeverity
  raisedAt: string
  ackedAt?: string | null
  resolvedAt?: string | null
  message: string
  deviceId: string
}

export interface NeighbourhoodUpdate {
  ts?: string | null
  segmentStatus?: SegmentStatus
  stormEvent?: StormEvent
  alert?: Alert
}

export interface Device {
  devEui: string
  kind: string
  installedAt?: string | null
}

export interface HomeHealth {
  lastSeen?: string | null
  battMv: number
  mainsOk: boolean
  baseflowCyclesPerDay: number
  activeAlerts: number
}

export interface Home {
  id: string
  segmentId: string
  pitAreaM2: number
  consentAt?: string | null
  devices: Device[]
  health?: HomeHealth | null
}

export interface HomeStormMetrics {
  stormId: string
  homeId: string
  lagMin: number
  recessionMin: number
  volumeL: number
  cycles: number
  lagReached: boolean
  recessionReached: boolean
  stormStartedAt?: string | null
}

export interface GetHomeResponse {
  home: Home
  recentStorms: HomeStormMetrics[]
}
