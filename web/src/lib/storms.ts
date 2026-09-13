import type { StormEvent } from './types'

// Oldest first: the replay gauge reads bottom (oldest) to top (newest).
export function chronological(storms: Iterable<StormEvent>): StormEvent[] {
  return [...storms].sort((a, b) => a.startedAt.localeCompare(b.startedAt) || a.id.localeCompare(b.id))
}

export function clampIndex(index: number, length: number): number {
  if (length <= 0) return -1
  if (!Number.isFinite(index)) return length - 1
  return Math.max(0, Math.min(length - 1, Math.round(index)))
}

// gaugeMarks places each storm on a 0..1 staff by rain total, for tick length.
export function gaugeMarks(storms: StormEvent[]): { id: string; position: number; weight: number }[] {
  if (storms.length === 0) return []
  const most = Math.max(...storms.map((s) => s.totalRainMm), 1)
  return storms.map((s, i) => ({
    id: s.id,
    position: storms.length === 1 ? 0.5 : i / (storms.length - 1),
    weight: Math.max(0.15, s.totalRainMm / most),
  }))
}

export function isOpen(s: StormEvent): boolean {
  return !s.endedAt
}
