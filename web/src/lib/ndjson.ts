// grpc-gateway streams server-streaming RPCs as newline-delimited JSON:
// one {"result": …} per message, or a final {"error": {code, message}}.

export type StreamEvent<T> =
  | { kind: 'result'; value: T }
  | { kind: 'error'; code: number; message: string }

export function parseLine<T>(line: string): StreamEvent<T> | null {
  const trimmed = line.trim()
  if (trimmed === '') return null
  const parsed = JSON.parse(trimmed) as { result?: T; error?: { code?: number; message?: string } }
  if (parsed.error) {
    return { kind: 'error', code: parsed.error.code ?? 2, message: parsed.error.message ?? 'stream error' }
  }
  if (parsed.result === undefined) throw new Error(`unexpected stream line: ${trimmed.slice(0, 80)}`)
  return { kind: 'result', value: parsed.result }
}

// NDJSONDecoder turns arbitrary text chunks into complete events.
export class NDJSONDecoder<T> {
  private buffer = ''

  push(chunk: string): StreamEvent<T>[] {
    this.buffer += chunk
    const lines = this.buffer.split('\n')
    this.buffer = lines.pop() ?? ''
    return lines.map((l) => parseLine<T>(l)).filter((e): e is StreamEvent<T> => e !== null)
  }

  flush(): StreamEvent<T>[] {
    const rest = this.buffer
    this.buffer = ''
    const e = parseLine<T>(rest)
    return e ? [e] : []
  }
}
