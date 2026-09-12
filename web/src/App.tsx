import { useCallback, useEffect, useMemo, useState } from 'react'
import { AccountButton, SignInControl } from './auth/AuthProvider'
import { useOwnerAuth } from './auth/useOwnerAuth'
import { AboutModal } from './components/AboutModal'
import { Legend } from './components/Legend'
import { MapView } from './components/MapView'
import { OwnerPanel } from './components/OwnerPanel'
import { SegmentPanel } from './components/SegmentPanel'
import { StormGauge } from './components/StormGauge'
import { getJSON } from './lib/api'
import { heatColor, niceMax } from './lib/heat'
import { liveView, stormView } from './lib/neighbourhood'
import { chronological, clampIndex } from './lib/storms'
import type { Segment, SegmentStormMetrics, StormEvent } from './lib/types'
import { useNeighbourhood } from './useNeighbourhood'

type Mode = 'live' | 'storm'
const NO_DATA_FILL = '#C9D3D8'

interface StormDetail {
  stormEvent: StormEvent
  segments: SegmentStormMetrics[]
}

export function App() {
  const auth = useOwnerAuth()
  // #storms links straight to the storm replay.
  const [mode, setModeState] = useState<Mode>(() => (window.location.hash === '#storms' ? 'storm' : 'live'))
  const setMode = (m: Mode) => {
    setModeState(m)
    window.history.replaceState(null, '', m === 'storm' ? '#storms' : window.location.pathname + window.location.search)
  }
  const [aboutOpen, setAboutOpen] = useState(false)
  const [segments, setSegments] = useState<Segment[] | null>(null)
  const [loadProblem, setLoadProblem] = useState<string | null>(null)
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const [listedStorms, setListedStorms] = useState<StormEvent[]>([])
  const [stormIndex, setStormIndex] = useState<number | null>(null) // null: newest
  const [details, setDetails] = useState<Record<string, StormDetail>>({})
  const { state, connection, problem } = useNeighbourhood(auth.getToken, auth.identity)

  useEffect(() => {
    const controller = new AbortController()
    Promise.all([
      getJSON<{ segments: Segment[] }>('/v1/segments', undefined, controller.signal),
      getJSON<{ stormEvents: StormEvent[] }>('/v1/storm-events?page_size=200', undefined, controller.signal),
    ])
      .then(([s, e]) => {
        setSegments(s.segments)
        setListedStorms(e.stormEvents)
      })
      .catch(() => {
        if (!controller.signal.aborted) setLoadProblem('The neighbourhood could not be loaded. Check that the api-gateway is running, then reload.')
      })
    return () => controller.abort()
  }, [])

  const storms = useMemo(() => {
    const byId = new Map<string, StormEvent>(listedStorms.map((s) => [s.id, s]))
    for (const s of Object.values(state.storms)) byId.set(s.id, s)
    return chronological(byId.values())
  }, [listedStorms, state.storms])
  const index = clampIndex(stormIndex ?? storms.length - 1, storms.length)
  const storm = index >= 0 ? storms[index] : undefined
  const stormKey = storm ? `${storm.id}:${storm.totalRainMm}:${storm.endedAt ?? ''}` : ''

  useEffect(() => {
    if (mode !== 'storm' || !storm || details[stormKey]) return
    const controller = new AbortController()
    getJSON<StormDetail>(`/v1/storm-events/${storm.id}`, undefined, controller.signal)
      .then((d) => setDetails((prev) => ({ ...prev, [stormKey]: d })))
      .catch(() => undefined) // the gauge still works; the map shows no data
    return () => controller.abort()
  }, [mode, storm, stormKey, details])

  const stormMetrics = useMemo(
    () => Object.fromEntries((details[stormKey]?.segments ?? []).map((m) => [m.segmentId, m])),
    [details, stormKey],
  )

  const list = useMemo(() => segments ?? [], [segments])
  const max = useMemo(() => {
    const views = list.map((s) => (mode === 'live' ? liveView(state.statuses[s.id]) : stormView(stormMetrics[s.id])))
    const values = views.flatMap((v) => (v.state === 'visible' ? [v.value] : []))
    return mode === 'live' ? niceMax(values, 2) : niceMax(values, 100)
  }, [list, mode, state.statuses, stormMetrics])

  const fillFor = useCallback(
    (id: string) => {
      const v = mode === 'live' ? liveView(state.statuses[id]) : stormView(stormMetrics[id])
      if (v.state === 'hidden') return null
      return v.state === 'visible' ? heatColor(v.value, max) : NO_DATA_FILL
    },
    [mode, state.statuses, stormMetrics, max],
  )

  const selected = list.find((s) => s.id === selectedId)
  const hasOutlines = list.some((s) => s.geometryGeojson)

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <h1>sumpnet</h1>
          <p>How Timberwalk’s streets handle stormwater</p>
        </div>
        <div className="mode" role="group" aria-label="Map view">
          <button type="button" aria-pressed={mode === 'live'} onClick={() => setMode('live')}>
            Live
          </button>
          <button type="button" aria-pressed={mode === 'storm'} onClick={() => setMode('storm')}>
            Storms
          </button>
        </div>
        <button type="button" className="quiet" onClick={() => setAboutOpen(true)}>
          About
        </button>
        {auth.configured && auth.loaded && (auth.signedIn ? <AccountButton /> : <SignInControl />)}
      </header>

      <main className="layout">
        <aside className="panel" aria-label="Details">
          {loadProblem && <p className="notice">{loadProblem}</p>}
          {mode === 'live' && problem && <p className="notice">{problem}</p>}
          <SegmentPanel
            mode={mode}
            segments={list}
            selected={selected}
            statuses={state.statuses}
            stormMetrics={stormMetrics}
            storm={storm}
            asOf={state.asOf}
            connection={connection}
            onSelect={setSelectedId}
          />
          <OwnerPanel
            configured={auth.configured}
            loaded={auth.loaded}
            signedIn={auth.signedIn}
            getToken={auth.getToken}
            streamAlerts={state.alerts}
            signIn={auth.configured ? <SignInControl /> : null}
          />
        </aside>

        <div className="map-wrap">
          <MapView segments={list} fillFor={fillFor} selectedId={selectedId} onSelect={setSelectedId} />
          {segments && !hasOutlines && (
            <div className="map-empty" role="status">
              <p>No street outlines are loaded yet, so the map has nothing to colour. An operator loads the demo streets with make seed.</p>
            </div>
          )}
          <Legend
            title={mode === 'live' ? 'Pump cycles per hour, per home' : 'Water pumped per home in this storm'}
            max={max}
            unit={mode === 'live' ? 'cycles/h' : 'L'}
          />
          {mode === 'storm' && <StormGauge storms={storms} index={index} onChange={(i) => setStormIndex(clampIndex(i, storms.length))} />}
        </div>
      </main>

      <AboutModal open={aboutOpen} onClose={() => setAboutOpen(false)} />
    </div>
  )
}
