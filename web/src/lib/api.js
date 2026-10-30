// Calls the rider endpoints of /api/v1 (see api/endpoints.md).

async function failure(res) {
  let msg = `${res.status} ${res.statusText}`
  try {
    msg = (await res.json()).error || msg
  } catch {}
  return new Error(msg)
}

async function json(res) {
  if (!res.ok) throw await failure(res)
  return res.json()
}

export const search = (q) => fetch(`/api/v1/search?${new URLSearchParams({ q, limit: '5' })}`).then(json)

const point = (p) => `${p.lat},${p.lng}`

export function quote({ startAddress, endAddress, start, end }) {
  const q = new URLSearchParams({ start_address: startAddress, end_address: endAddress })
  if (start) q.set('start', point(start))
  if (end) q.set('end', point(end))
  return fetch(`/api/v1/ride?${q}`).then(json)
}

export function requestRide(body) {
  return fetch('/api/v1/ride', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  }).then(json)
}

// getRide returns { ride } once it can be shown, { payment } with the x402
// requirements while the fare is due, and { settlement } alongside a failed
// or successful payment attempt.
export async function getRide(id, paymentSignature) {
  const headers = paymentSignature ? { 'PAYMENT-SIGNATURE': paymentSignature.trim() } : {}
  const res = await fetch(`/api/v1/ride/${encodeURIComponent(id)}`, { headers })
  let settlement
  const header = res.headers.get('PAYMENT-RESPONSE')
  if (header) {
    try {
      settlement = JSON.parse(atob(header))
    } catch {}
  }
  if (res.status === 402) {
    const body = await res.json().catch(() => ({}))
    return { payment: body.accepts ? body : null, settlement, reason: body.error }
  }
  if (!res.ok) throw await failure(res)
  return { ride: await res.json(), settlement }
}

const units = {
  cardano: { symbol: 'ADA', decimals: 6, unit: 'lovelace' },
  bsv: { symbol: 'BSV', decimals: 8, unit: 'sat' },
}

export const chainOf = (network) => (network?.startsWith('bsv:') ? 'bsv' : 'cardano')

// money formats an amount in a network's smallest unit, e.g. 3505000 on
// cardano:preprod as "3.505 ADA".
export function money(amount, network) {
  const u = units[chainOf(network)]
  const value = Number(amount) / 10 ** u.decimals
  return `${value.toLocaleString(undefined, { maximumFractionDigits: u.decimals })} ${u.symbol}`
}

export function short(s, n = 10) {
  return s && s.length > 2 * n + 1 ? `${s.slice(0, n)}…${s.slice(-n)}` : s
}
