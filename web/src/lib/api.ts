import { NDJSONDecoder } from './ndjson'
import type { NeighbourhoodUpdate } from './types'

// A TokenGetter returns a fresh Clerk session token, or null when signed out.
export type TokenGetter = () => Promise<string | null>

export class ApiError extends Error {
  readonly status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

// Same-origin /v1 (the Vite proxy) unless VITE_API_BASE_URL points elsewhere.
export const API_BASE: string = (import.meta.env.VITE_API_BASE_URL ?? '').replace(/\/$/, '')

async function headers(getToken?: TokenGetter): Promise<HeadersInit> {
  const h: Record<string, string> = { Accept: 'application/json' }
  const token = getToken ? await getToken() : null
  if (token) h.Authorization = `Bearer ${token}`
  return h
}

async function failure(res: Response): Promise<ApiError> {
  let message = res.statusText
  try {
    const body = (await res.json()) as { message?: string }
    if (body.message) message = body.message
  } catch {
    // non-JSON error body: keep the status text
  }
  return new ApiError(res.status, message)
}

export async function getJSON<T>(path: string, getToken?: TokenGetter, signal?: AbortSignal): Promise<T> {
  const res = await fetch(API_BASE + path, { headers: await headers(getToken), signal })
  if (!res.ok) throw await failure(res)
  return (await res.json()) as T
}

export async function postJSON<T>(path: string, body: unknown, getToken?: TokenGetter): Promise<T> {
  const h = { ...(await headers(getToken)), 'Content-Type': 'application/json' }
  const res = await fetch(API_BASE + path, { method: 'POST', headers: h, body: JSON.stringify(body) })
  if (!res.ok) throw await failure(res)
  return (await res.json()) as T
}

// watchNeighbourhood reads the NDJSON stream until it ends or signal aborts.
export async function watchNeighbourhood(opts: {
  signal: AbortSignal
  getToken?: TokenGetter
  onOpen?: () => void
  onUpdate: (u: NeighbourhoodUpdate) => void
}): Promise<void> {
  const res = await fetch(`${API_BASE}/v1/neighbourhood:watch?send_snapshot=true`, {
    headers: await headers(opts.getToken),
    signal: opts.signal,
  })
  if (!res.ok || !res.body) throw await failure(res)
  opts.onOpen?.()
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
  const decoder = new NDJSONDecoder<{ update: NeighbourhoodUpdate }>()
  const handle = (events: ReturnType<typeof decoder.push>) => {
    for (const e of events) {
      if (e.kind === 'error') throw new ApiError(502, e.message)
      opts.onUpdate(e.value.update)
    }
  }
  for (;;) {
    const { value, done } = await reader.read()
    if (done) break
    handle(decoder.push(value))
  }
  handle(decoder.flush())
}

// reconnectDelay: exponential backoff from 1 s, capped at 30 s, with jitter.
export function reconnectDelay(attempt: number, random: () => number = Math.random): number {
  const base = Math.min(30_000, 1000 * 2 ** Math.max(0, attempt))
  return Math.round(base / 2 + (random() * base) / 2)
}
