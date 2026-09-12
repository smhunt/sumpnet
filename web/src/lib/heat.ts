// Sequential water scale, dry to surcharged. Hidden streets never use it.
export const WATER_SCALE = ['#D7E3DC', '#9CC5C4', '#5FA3B8', '#2F7391', '#1B4E6B', '#0A2F45'] as const

// niceMax rounds the scale top up to 1, 2 or 5 × 10^n so legends read well.
export function niceMax(values: number[], floor = 1): number {
  const top = Math.max(floor, ...values.filter((v) => Number.isFinite(v)))
  const exp = Math.pow(10, Math.floor(Math.log10(top)))
  const f = top / exp
  const step = f <= 1 ? 1 : f <= 2 ? 2 : f <= 5 ? 5 : 10
  return step * exp
}

export function bucket(value: number, max: number): number {
  if (!(max > 0) || !(value > 0)) return 0
  const t = Math.min(1, value / max)
  return Math.min(WATER_SCALE.length - 1, Math.floor(t * WATER_SCALE.length))
}

export function heatColor(value: number, max: number): string {
  return WATER_SCALE[bucket(value, max)] ?? WATER_SCALE[0]
}
