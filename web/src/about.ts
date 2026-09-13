// In-app documentation: version, changelog, how it works and roadmap.
// Keep CHANGELOG in step with the repository's CHANGELOG.md.

export const APP_VERSION = '0.5.0'
export const REPOSITORY_URL = 'https://github.com/smhunt/sumpnet'

export interface ChangelogEntry {
  version: string
  date: string
  changes: string[]
}

export const CHANGELOG: ChangelogEntry[] = [
  {
    version: '0.5.0',
    date: '2026-09-12',
    changes: [
      'Neighbourhood map with live pump activity per street, streamed from the api-gateway',
      'Streets with fewer than three reporting homes are hidden and explained, never shown as zero',
      'Storm replay: step through recorded storms and compare litres pumped per home by street',
      'Owner sign-in with Clerk: your own home, sensor health, recent storms and alerts',
      'Acknowledge your own alerts from the dashboard',
    ],
  },
  {
    version: '0.4.0',
    date: '2026-09-12',
    changes: [
      'Rainfall from two neighbourhood rain gauges, with Environment Canada hourly observations filling any gaps',
      'Storms found automatically, with how fast each home responded, how long it took to settle and how much water was pumped',
      'Power-outage risk alerts now take recent rain into account',
    ],
  },
  {
    version: '0.3.0',
    date: '2026-09-11',
    changes: ['Pump-cycle detection (dry run, short cycling, continuous run) and alert email'],
  },
  {
    version: '0.2.0',
    date: '2026-09-11',
    changes: ['Ingest path: LoRaWAN and Wi-Fi sensor data stored without loss or duplicates'],
  },
  {
    version: '0.1.0',
    date: '2026-09-11',
    changes: ['Sensor payload contracts and a deterministic 60-home neighbourhood simulator'],
  },
  {
    version: '0.0.1',
    date: '2026-09-11',
    changes: ['Project scaffold: services, local stack and continuous integration'],
  },
]

export interface HowItWorksStep {
  title: string
  description: string
}

export const HOW_IT_WORKS: HowItWorksStep[] = [
  {
    title: 'A sensor watches each sump pit',
    description:
      'Volunteer homes fit a small node that measures the water level and senses pump current through a plug-in clamp. Pump wiring is never touched.',
  },
  {
    title: 'Streets, not houses',
    description:
      'The public map only shows a street once at least three homes on it report. Below that it stays hidden, because a single pump can reveal who is home or whose basement floods.',
  },
  {
    title: 'Rain gauges time every storm',
    description:
      'Two rain gauges at opposite ends of the neighbourhood record rainfall every few minutes, and Environment Canada’s hourly observations from London fill any gaps. A storm’s start and end come from that record.',
  },
  {
    title: 'Live, or storm by storm',
    description:
      'Live mode shows pump cycles per hour per home over the last hour of readings. Storm mode shows how much water each street pumped per home during a recorded storm, how fast pumps responded and how long they took to settle.',
  },
  {
    title: 'Your own home, only to you',
    description:
      'Sign in to see your home’s sensor health, recent storms and alerts. The operator links your account to your home; nobody else can see it.',
  },
]

export type Priority = 'high' | 'medium' | 'low'

export interface RoadmapGroup {
  category: string
  items: { title: string; priority: Priority }[]
}

export const ROADMAP: RoadmapGroup[] = [
  {
    category: 'In progress',
    items: [
      { title: 'Check a live storm replay on the map against the running stack', priority: 'high' },
      { title: 'Replay this summer’s storms over simulated homes on Timberwalk’s real streets', priority: 'high' },
    ],
  },
  {
    category: 'Proposed (owner decision pending)',
    items: [
      { title: 'Bucket test measures each pit’s real size, instead of setting the pump rate', priority: 'high' },
      { title: 'Guided pump calibration in the dashboard', priority: 'medium' },
      { title: 'Warnings for a failing check valve or a weakening pump', priority: 'medium' },
      { title: 'Calibration mode and finer level sampling on the node', priority: 'low' },
    ],
  },
  {
    category: 'Planned',
    items: [
      { title: 'Ask Claude about storms through the MCP server', priority: 'high' },
      { title: 'First real sensors installed alongside the simulated homes', priority: 'high' },
      { title: 'Consent form and plain-language data policy for pilot homes', priority: 'high' },
      { title: 'Cloud deployment and published load-test results', priority: 'medium' },
      { title: 'Show storm inflow from each pump’s own flow rate in the owner view', priority: 'medium' },
      { title: 'Surveyed street outlines agreed with Middlesex Centre', priority: 'medium' },
      { title: 'Alert email to each owner instead of one operator', priority: 'low' },
    ],
  },
]
