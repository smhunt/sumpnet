import { useEffect, useRef, useState } from 'react'
import { APP_VERSION, CHANGELOG, HOW_IT_WORKS, REPOSITORY_URL, ROADMAP } from '../about'

type Tab = 'changelog' | 'how' | 'roadmap'

const TABS: { id: Tab; label: string }[] = [
  { id: 'how', label: 'How it works' },
  { id: 'changelog', label: 'Changelog' },
  { id: 'roadmap', label: 'Roadmap' },
]

interface Props {
  open: boolean
  onClose: () => void
}

export function AboutModal({ open, onClose }: Props) {
  const ref = useRef<HTMLDialogElement>(null)
  const [tab, setTab] = useState<Tab>('how')

  useEffect(() => {
    const d = ref.current
    if (!d) return
    if (open && !d.open) d.showModal()
    if (!open && d.open) d.close()
  }, [open])

  return (
    <dialog ref={ref} className="about" onClose={onClose} aria-labelledby="about-title">
      <header className="about-head">
        <h2 id="about-title">
          About sumpnet <span className="version">v{APP_VERSION}</span>
        </h2>
        <button type="button" className="quiet" onClick={onClose}>
          Close
        </button>
      </header>
      <div role="tablist" aria-label="About sections" className="tabs">
        {TABS.map((t) => (
          <button
            key={t.id}
            role="tab"
            type="button"
            id={`tab-${t.id}`}
            aria-selected={tab === t.id}
            aria-controls={`panel-${t.id}`}
            onClick={() => setTab(t.id)}
          >
            {t.label}
          </button>
        ))}
      </div>
      <div role="tabpanel" id={`panel-${tab}`} aria-labelledby={`tab-${tab}`} className="about-body">
        {tab === 'how' && (
          <ol className="how">
            {HOW_IT_WORKS.map((s) => (
              <li key={s.title}>
                <h3>{s.title}</h3>
                <p>{s.description}</p>
              </li>
            ))}
          </ol>
        )}
        {tab === 'changelog' &&
          CHANGELOG.map((c) => (
            <section key={c.version} className="release">
              <h3>
                {c.version} <time dateTime={c.date}>{c.date}</time>
              </h3>
              <ul>
                {c.changes.map((x) => (
                  <li key={x}>{x}</li>
                ))}
              </ul>
            </section>
          ))}
        {tab === 'roadmap' &&
          ROADMAP.map((g) => (
            <section key={g.category} className="release">
              <h3>{g.category}</h3>
              <ul>
                {g.items.map((i) => (
                  <li key={i.title}>
                    {i.title} <span className={`priority priority-${i.priority}`}>{i.priority} priority</span>
                  </li>
                ))}
              </ul>
            </section>
          ))}
      </div>
      <footer className="about-foot">
        Source: <a href={REPOSITORY_URL}>{REPOSITORY_URL.replace('https://', '')}</a> (private)
      </footer>
    </dialog>
  )
}
