import { WATER_SCALE } from '../lib/heat'
import { formatNumber } from '../lib/format'

interface Props {
  title: string
  max: number
  unit: string
}

export function Legend({ title, max, unit }: Props) {
  const step = max / WATER_SCALE.length
  return (
    <figure className="legend" aria-label={`Map legend: ${title}`}>
      <figcaption>{title}</figcaption>
      <ol className="legend-scale">
        {WATER_SCALE.map((c, i) => (
          <li key={c} style={{ background: c }} title={`${formatNumber(i * step)}–${formatNumber((i + 1) * step)} ${unit}`} />
        ))}
      </ol>
      <div className="legend-ends">
        <span>0</span>
        <span>
          {formatNumber(max, max < 10 ? 1 : 0)} {unit}
        </span>
      </div>
      <p className="legend-hidden">
        <span className="swatch-hatch" aria-hidden="true" /> Hidden: fewer than 3 homes report
      </p>
      <p className="legend-note">Street outlines are approximate, not surveyed lot lines.</p>
    </figure>
  )
}
