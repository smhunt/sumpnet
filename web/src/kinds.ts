import type { SegmentKind } from './lib/types'

export const KIND_LABEL: Record<SegmentKind, string> = {
  SEGMENT_KIND_UNSPECIFIED: 'Street',
  SEGMENT_KIND_STANDARD: 'Standard lots',
  SEGMENT_KIND_WOODED: 'Wooded lots',
  SEGMENT_KIND_NEAR_POND: 'Near the stormwater pond',
  SEGMENT_KIND_HIGH_GROUND: 'High ground',
}
