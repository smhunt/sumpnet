import type { CSSProperties } from 'react'
import { formatDate, formatNumber } from '../lib/format'
import { gaugeMarks, isOpen } from '../lib/storms'
import type { StormEvent } from '../lib/types'

interface Props {
  storms: StormEvent[] // oldest first
  index: number
  onChange: (index: number) => void
}

// StormGauge is the replay control, drawn as a staff gauge: one graduation
// per recorded storm, its length set by the storm's rain total, oldest at the
// bottom. A native range input underneath keeps it keyboard-operable.
export function StormGauge({ storms, index, onChange }: Props) {
  if (storms.length === 0) {
    return (
      <div className="gauge gauge-empty" role="status">
        <p>No storms recorded yet.</p>
        <p className="muted">Storms appear here once rainfall has been measured and analysed.</p>
      </div>
    )
  }
  const marks = gaugeMarks(storms)
  const current = storms[index]
  return (
    <div className="gauge">
      <div className="gauge-readout" aria-live="polite">
        {current && (
          <>
            <strong>{formatDate(current.startedAt)}</strong>
            <span>{formatNumber(current.totalRainMm, 0)} mm of rain</span>
            {isOpen(current) && <span className="badge">Still raining</span>}
          </>
        )}
      </div>
      <div className="gauge-staff">
        <span className="gauge-end">{formatDate(storms[storms.length - 1]?.startedAt)}</span>
        <div className="gauge-track">
          {marks.map((m, i) => (
            <button
              key={m.id}
              type="button"
              tabIndex={-1}
              className={i === index ? 'gauge-mark is-current' : 'gauge-mark'}
              style={{ '--pos': m.position, '--weight': m.weight } as CSSProperties}
              onClick={() => onChange(i)}
              aria-hidden="true"
            />
          ))}
          <input
            className="gauge-input"
            type="range"
            min={0}
            max={storms.length - 1}
            step={1}
            value={index}
            onChange={(e) => onChange(Number(e.target.value))}
            aria-label="Choose a storm to replay"
            aria-valuetext={current ? `${formatDate(current.startedAt)}, ${formatNumber(current.totalRainMm, 0)} mm` : undefined}
          />
        </div>
        <span className="gauge-end">{formatDate(storms[0]?.startedAt)}</span>
      </div>
      <div className="gauge-steps">
        <button type="button" onClick={() => onChange(index - 1)} disabled={index <= 0}>
          Earlier
        </button>
        <button type="button" onClick={() => onChange(index + 1)} disabled={index >= storms.length - 1}>
          Later
        </button>
      </div>
    </div>
  )
}
