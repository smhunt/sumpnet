import { useEffect, useReducer, useRef, useState } from 'react'
import { ApiError, reconnectDelay, watchNeighbourhood, type TokenGetter } from './lib/api'
import { applyUpdate, emptyNeighbourhood, type NeighbourhoodState } from './lib/neighbourhood'
import type { NeighbourhoodUpdate } from './lib/types'

export type Connection = 'connecting' | 'live' | 'retrying'

type Action = { type: 'update'; u: NeighbourhoodUpdate } | { type: 'opened' }

function reducer(state: NeighbourhoodState, a: Action): NeighbourhoodState {
  if (a.type === 'opened') return { ...state, alerts: {} } // the snapshot resends active alerts
  return applyUpdate(state, a.u)
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms)
    signal.addEventListener('abort', () => {
      clearTimeout(t)
      resolve()
    })
  })
}

// useNeighbourhood keeps one WatchNeighbourhood stream open, reconnecting with
// backoff. identity changes (sign-in, sign-out) reopen it so the gateway
// routes the right owner's alerts.
export function useNeighbourhood(getToken: TokenGetter, identity: string) {
  const [state, dispatch] = useReducer(reducer, emptyNeighbourhood)
  const [connection, setConnection] = useState<Connection>('connecting')
  const [problem, setProblem] = useState<string | null>(null)
  const tokenRef = useRef(getToken)
  useEffect(() => {
    tokenRef.current = getToken
  }, [getToken])

  useEffect(() => {
    const controller = new AbortController()
    const { signal } = controller
    void (async () => {
      let attempt = 0
      while (!signal.aborted) {
        setConnection(attempt === 0 ? 'connecting' : 'retrying')
        try {
          await watchNeighbourhood({
            signal,
            getToken: () => tokenRef.current(),
            onOpen: () => {
              attempt = 0
              setProblem(null)
              setConnection('live')
              dispatch({ type: 'opened' })
            },
            onUpdate: (u) => dispatch({ type: 'update', u }),
          })
        } catch (err) {
          if (signal.aborted) return
          if (err instanceof ApiError && err.status === 401) setProblem('Your session has expired. Sign in again to see your alerts.')
          else setProblem('Live updates are interrupted. Reconnecting.')
        }
        if (signal.aborted) return
        setConnection('retrying')
        await sleep(reconnectDelay(attempt++), signal)
      }
    })()
    return () => controller.abort()
  }, [identity])

  return { state, connection, problem }
}
