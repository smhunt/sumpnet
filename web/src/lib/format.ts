const zone = 'America/Toronto'

const dateTime = new Intl.DateTimeFormat('en-CA', { timeZone: zone, dateStyle: 'medium', timeStyle: 'short' })
const dateOnly = new Intl.DateTimeFormat('en-CA', { timeZone: zone, day: 'numeric', month: 'short', year: 'numeric' })

export function formatDateTime(iso: string | null | undefined): string {
  return iso ? dateTime.format(new Date(iso)) : 'never'
}

export function formatDate(iso: string | null | undefined): string {
  return iso ? dateOnly.format(new Date(iso)) : ''
}

export function formatNumber(v: number, digits = 1): string {
  return v.toLocaleString('en-CA', { minimumFractionDigits: digits, maximumFractionDigits: digits })
}

export function formatMinutes(min: number): string {
  if (min < 90) return `${Math.round(min)} min`
  return `${formatNumber(min / 60)} h`
}
