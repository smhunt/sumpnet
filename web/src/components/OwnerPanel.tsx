import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { ApiError, getJSON, postJSON, type TokenGetter } from '../lib/api'
import { formatDate, formatDateTime, formatMinutes, formatNumber } from '../lib/format'
import { alertTitle, sortAlerts } from '../lib/neighbourhood'
import type { Alert, GetHomeResponse, Home } from '../lib/types'

interface Props {
  configured: boolean
  loaded: boolean
  signedIn: boolean
  getToken: TokenGetter
  streamAlerts: Record<string, Alert>
  signIn: ReactNode
}

export function OwnerPanel({ configured, loaded, signedIn, getToken, streamAlerts, signIn }: Props) {
  if (!configured) {
    return (
      <section className="panel-section" aria-labelledby="owner-title">
        <h2 id="owner-title">Your home</h2>
        <p className="muted">Owner sign-in is not set up on this dashboard yet.</p>
      </section>
    )
  }
  if (!loaded) {
    return (
      <section className="panel-section" aria-labelledby="owner-title">
        <h2 id="owner-title">Your home</h2>
        <p className="muted">Checking sign-in.</p>
      </section>
    )
  }
  if (!signedIn) {
    return (
      <section className="panel-section" aria-labelledby="owner-title">
        <h2 id="owner-title">Your home</h2>
        <p>If your home takes part, sign in to see your own pump, sensor health and alerts. Nobody else can see them.</p>
        <div>{signIn}</div>
      </section>
    )
  }
  return <OwnerHome getToken={getToken} streamAlerts={streamAlerts} />
}

function OwnerHome({ getToken, streamAlerts }: { getToken: TokenGetter; streamAlerts: Record<string, Alert> }) {
  const tokenRef = useRef(getToken)
  useEffect(() => {
    tokenRef.current = getToken
  }, [getToken])
  const [homes, setHomes] = useState<Home[] | null>(null)
  const [detail, setDetail] = useState<GetHomeResponse | null>(null)
  const [fetched, setFetched] = useState<Record<string, Alert>>({})
  const [problem, setProblem] = useState<string | null>(null)
  const [busy, setBusy] = useState<string | null>(null)

  useEffect(() => {
    const controller = new AbortController()
    const token = () => tokenRef.current()
    void (async () => {
      try {
        const [mine, alerts] = await Promise.all([
          getJSON<{ homes: Home[] }>('/v1/me/homes', token, controller.signal),
          getJSON<{ alerts: Alert[] }>('/v1/me/alerts', token, controller.signal),
        ])
        setHomes(mine.homes)
        setFetched(Object.fromEntries(alerts.alerts.map((a) => [a.id, a])))
        const first = mine.homes[0]
        if (first) setDetail(await getJSON<GetHomeResponse>(`/v1/homes/${first.id}`, token, controller.signal))
      } catch (err) {
        if (controller.signal.aborted) return
        setProblem(err instanceof ApiError && err.status === 401 ? 'Your session has expired. Sign in again.' : 'Your home could not be loaded. Try again shortly.')
      }
    })()
    return () => controller.abort()
  }, [])

  // The stream is newer than the initial fetch; it also drops resolved alerts.
  const alerts = useMemo(() => {
    const merged: Record<string, Alert> = { ...fetched, ...streamAlerts }
    return sortAlerts(Object.values(merged).filter((a) => !a.resolvedAt))
  }, [fetched, streamAlerts])

  async function acknowledge(id: string) {
    setBusy(id)
    try {
      const res = await postJSON<{ alert: Alert }>(`/v1/me/alerts/${id}:acknowledge`, {}, tokenRef.current)
      setFetched((f) => ({ ...f, [id]: res.alert }))
      setProblem(null)
    } catch {
      setProblem('The alert could not be acknowledged. Try again.')
    } finally {
      setBusy(null)
    }
  }

  if (problem && !homes) {
    return (
      <section className="panel-section">
        <h2>Your home</h2>
        <p className="notice">{problem}</p>
      </section>
    )
  }
  if (!homes) {
    return (
      <section className="panel-section">
        <h2>Your home</h2>
        <p className="muted">Loading your home.</p>
      </section>
    )
  }
  if (homes.length === 0) {
    return (
      <section className="panel-section">
        <h2>Your home</h2>
        <p>Your account is not linked to a home yet. Ask the sumpnet operator to link it once your sensor is installed.</p>
      </section>
    )
  }

  const home = detail?.home ?? homes[0]
  const health = home?.health
  return (
    <>
      <section className="panel-section" aria-labelledby="home-title">
        <h2 id="home-title">Your home</h2>
        {problem && <p className="notice">{problem}</p>}
        <dl className="readings">
          <div>
            <dt>Last sensor reading</dt>
            <dd>{formatDateTime(health?.lastSeen)}</dd>
          </div>
          <div>
            <dt>Mains power at the pump</dt>
            <dd>{health?.lastSeen ? (health.mainsOk ? 'On' : 'Off') : 'Unknown'}</dd>
          </div>
          <div>
            <dt>Sensor battery</dt>
            <dd>{health?.battMv ? `${formatNumber(health.battMv / 1000, 2)} V` : 'Unknown'}</dd>
          </div>
          <div>
            <dt>Dry-weather pump cycles per day</dt>
            <dd>{health?.baseflowCyclesPerDay ? formatNumber(health.baseflowCyclesPerDay) : 'Not measured yet'}</dd>
          </div>
          <div>
            <dt>Sensors</dt>
            <dd>{home?.devices.length ?? 0}</dd>
          </div>
        </dl>
        {homes.length > 1 && <p className="muted">Showing the first of your {homes.length} homes.</p>}
      </section>

      <section className="panel-section" aria-labelledby="alerts-title">
        <h2 id="alerts-title">Your alerts</h2>
        {alerts.length === 0 ? (
          <p className="muted">No active alerts. You will see them here the moment a sensor raises one.</p>
        ) : (
          <ul className="alerts">
            {alerts.map((a) => (
              <li key={a.id} className="alert" data-severity={a.severity} data-acked={Boolean(a.ackedAt)}>
                <strong>{alertTitle(a.code)}</strong>
                <span className="muted">Raised {formatDateTime(a.raisedAt)}</span>
                <div className="alert-actions">
                  {a.ackedAt ? (
                    <span className="muted">Acknowledged {formatDateTime(a.ackedAt)}</span>
                  ) : (
                    <button type="button" className="button" disabled={busy === a.id} onClick={() => void acknowledge(a.id)}>
                      {busy === a.id ? 'Acknowledging' : 'Acknowledge'}
                    </button>
                  )}
                </div>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="panel-section" aria-labelledby="storms-title">
        <h2 id="storms-title">Your recent storms</h2>
        {detail && detail.recentStorms.length > 0 ? (
          <table className="storm-table">
            <thead>
              <tr>
                <th scope="col">Storm</th>
                <th scope="col">Pumped</th>
                <th scope="col">Response</th>
                <th scope="col">Settled</th>
              </tr>
            </thead>
            <tbody>
              {detail.recentStorms.map((s) => (
                <tr key={s.stormId}>
                  <td>{formatDate(s.stormStartedAt)}</td>
                  <td>{formatNumber(s.volumeL, 0)} L</td>
                  <td>{s.lagReached ? formatMinutes(s.lagMin) : 'Not reached'}</td>
                  <td>{s.recessionReached ? formatMinutes(s.recessionMin) : 'Not yet'}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : (
          <p className="muted">No storms have been analysed for your home yet.</p>
        )}
      </section>
    </>
  )
}
