// Klien API KarhutlaSQL (/api/v1/*). Token disimpan di sessionStorage
// (hilang saat tab ditutup); password tidak pernah disimpan.
import { tableFromIPC, Type } from 'apache-arrow'

const KEY = 'karhutlasql.token'

export const session = {
  get token() { try { return sessionStorage.getItem(KEY) } catch { return null } },
  set token(v) { try { v ? sessionStorage.setItem(KEY, v) : sessionStorage.removeItem(KEY) } catch { /* private mode */ } },
}

export class ApiError extends Error {
  constructor(status, message) { super(message); this.status = status }
}

async function fail(res) {
  let msg = res.statusText
  try { msg = (await res.json()).error || msg } catch { /* bukan JSON */ }
  if (res.status === 401) session.token = null
  return new ApiError(res.status, msg)
}

export async function login(user, password) {
  const res = await fetch('api/v1/login', {
    method: 'POST',
    headers: { Authorization: 'Basic ' + btoa(unescape(encodeURIComponent(`${user}:${password}`))) },
  })
  if (!res.ok) throw await fail(res)
  session.token = (await res.json()).token
}

function auth() {
  return { Authorization: `Bearer ${session.token}` }
}

export async function tables() {
  const res = await fetch('api/v1/tables', { headers: auth() })
  if (!res.ok) throw await fail(res)
  return res.json()
}

// query → { columns: [{name, type}], rows: [[...]], rowCount, ms }
export async function query(sql, params = [], signal) {
  const t0 = performance.now()
  const res = await fetch('api/v1/query', {
    method: 'POST',
    headers: { ...auth(), 'Content-Type': 'application/json' },
    body: JSON.stringify({ sql, params }),
    signal,
  })
  if (!res.ok) throw await fail(res)
  const table = await tableFromIPC(res)
  const columns = table.schema.fields.map((f) => ({ name: f.name, type: String(f.type), format: formatter(f.type) }))
  const rows = []
  for (const row of table) {
    rows.push(columns.map((c) => {
      const v = row[c.name]
      return v === null || v === undefined ? null : c.format(v)
    }))
  }
  return { columns, rows, rowCount: table.numRows, ms: Math.round(performance.now() - t0) }
}

// formatter mengubah nilai Arrow JS menjadi teks tampilan.
function formatter(type) {
  switch (type.typeId) {
    case Type.Decimal:
      return (v) => decimal(v, type.scale)
    case Type.Timestamp:
      return (v) => new Date(Number(v)).toISOString().replace('T', ' ').replace(/\.000Z$|Z$/, '')
    case Type.Date:
      return (v) => new Date(Number(v)).toISOString().slice(0, 10)
    case Type.Binary:
    case Type.LargeBinary:
      return (v) => '\\x' + Array.from(v, (b) => b.toString(16).padStart(2, '0')).join('')
    case Type.List:
    case Type.FixedSizeList:
    case Type.Struct:
    case Type.Map:
      return (v) => JSON.stringify(v.toJSON ? v.toJSON() : v, (_, x) => (typeof x === 'bigint' ? x.toString() : x))
    default:
      return (v) => (typeof v === 'bigint' ? v.toString() : String(v))
  }
}

// decimal: nilai Decimal Arrow JS (bilangan bulat besar) → teks dengan skala.
export function decimal(v, scale) {
  let s = String(typeof v === 'bigint' ? v : BigInt(v.toString()))
  const neg = s.startsWith('-')
  if (neg) s = s.slice(1)
  if (scale > 0) {
    s = s.padStart(scale + 1, '0')
    s = s.slice(0, -scale) + '.' + s.slice(-scale)
  }
  return (neg ? '-' : '') + s
}
