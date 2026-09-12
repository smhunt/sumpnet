import { KIND_LABEL } from '../kinds'
import { formatDateTime, formatMinutes, formatNumber } from '../lib/format'
import { hiddenReason, liveView, stormView } from '../lib/neighbourhood'
import type { Segment, SegmentStatus, SegmentStormMetrics, StormEvent } from '../lib/types'
import type { Connection } from '../useNeighbourhood'

interface Props {
  mode: 'live' | 'storm'
  segments: Segment[]
  selected: Segment | undefined
  statuses: Record<string, SegmentStatus>
  stormMetrics: Record<string, SegmentStormMetrics>
  storm: StormEvent | undefined
  asOf: string | null
  connection: Connection
  onSelect: (id: string | null) => void
}

export function SegmentPanel({ mode, segments, selected, statuses, stormMetrics, storm, asOf, connection, onSelect }: Props) {
  if (!selected) {
    const views = segments.map((s) => (mode === 'live' ? liveView(statuses[s.id]) : stormView(stormMetrics[s.id])))
    const visible = views.filter((v) => v.state === 'visible').length
    return (
      <section className="panel-section" aria-labelledby="overview-title">
        <h2 id="overview-title">{mode === 'live' ? 'Right now' : 'This storm'}</h2>
        {mode === 'live' ? (
          <p className="lede">
            <span className="live-dot" data-state={connection} aria-hidden="true" />
            {connection === 'live' ? 'Live' : connection === 'connecting' ? 'Connecting' : 'Reconnecting'}
            {asOf ? `, readings to ${formatDateTime(asOf)}.` : '.'}
          </p>
        ) : (
          <p className="lede">{storm ? `Storm starting ${formatDateTime(storm.startedAt)}.` : 'Pick a storm on the gauge.'}</p>
        )}
        <p>
          {visible} of {segments.length} streets have enough reporting homes to show. Choose a street for details.
        </p>
        <ul className="street-list">
          {segments.map((s, i) => {
            const v = views[i]
            const value =
              v?.state === 'visible'
                ? mode === 'live'
                  ? `${formatNumber(v.value)} cycles/h`
                  : `${formatNumber(v.value, 0)} L`
                : v?.state === 'hidden'
                  ? 'Hidden'
                  : 'No data'
            return (
              <li key={s.id}>
                <button type="button" onClick={() => onSelect(s.id)}>
                  <span>{s.name}</span>
                  <span className={v?.state === 'visible' ? '' : 'muted'}>{value}</span>
                </button>
              </li>
            )
          })}
        </ul>
      </section>
    )
  }

  const kind = KIND_LABEL[selected.kind]
  const header = (
    <header className="segment-head">
      <div>
        <h2>{selected.name}</h2>
        <p className="muted">
          {kind}, {selected.homeCount} {selected.homeCount === 1 ? 'home' : 'homes'} taking part
        </p>
      </div>
      <button type="button" className="quiet" onClick={() => onSelect(null)}>
        Close
      </button>
    </header>
  )

  if (mode === 'live') {
    const s = statuses[selected.id]
    const v = liveView(s)
    return (
      <section className="panel-section">
        {header}
        {v.state === 'visible' && s ? (
          <dl className="readings">
            <div>
              <dt>Pump cycles per hour, per home</dt>
              <dd>{formatNumber(v.value)}</dd>
            </div>
            <div>
              <dt>Homes reporting</dt>
              <dd>{v.homesReporting}</dd>
            </div>
            <div>
              <dt>Active alerts on this street</dt>
              <dd>{s.activeAlerts}</dd>
            </div>
          </dl>
        ) : (
          <p className="hidden-note">{v.state === 'hidden' ? hiddenReason(v.homesReporting) : 'No readings from this street yet.'}</p>
        )}
      </section>
    )
  }

  const m = stormMetrics[selected.id]
  const v = stormView(m)
  return (
    <section className="panel-section">
      {header}
      {v.state === 'visible' && m ? (
        <dl className="readings">
          <div>
            <dt>Water pumped per home</dt>
            <dd>{formatNumber(v.value, 0)} L</dd>
          </div>
          <div>
            <dt>Typical response after rain began</dt>
            <dd>{m.medianLagMin > 0 ? formatMinutes(m.medianLagMin) : 'No pump reached storm pace'}</dd>
          </div>
          <div>
            <dt>Typical time to settle after rain ended</dt>
            <dd>{m.medianRecessionMin > 0 ? formatMinutes(m.medianRecessionMin) : 'Not settled yet'}</dd>
          </div>
          <div>
            <dt>Homes reporting</dt>
            <dd>{v.homesReporting}</dd>
          </div>
        </dl>
      ) : (
        <p className="hidden-note">{v.state === 'hidden' ? hiddenReason(v.homesReporting) : 'No storm data for this street.'}</p>
      )}
    </section>
  )
}
