export type APIErrorBody = { code: string; message: string; request_id?: string }
export class APIError extends Error {
  constructor(public status: number, public detail: APIErrorBody) { super(detail.message) }
}
let csrf = ''
export const setCSRF = (value: string) => { csrf = value }

export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers)
  const unsafe = !['GET', 'HEAD', 'OPTIONS'].includes((init.method ?? 'GET').toUpperCase())
  if (unsafe && csrf) headers.set('X-CSRF-Token', csrf)
  if (init.body && !(init.body instanceof FormData) && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json')
  const response = await fetch(path.startsWith('/auth/') ? path : `/api/v1${path}`, { ...init, headers, credentials: 'same-origin' })
  if (!response.ok) {
    const body = await response.json().catch(() => ({ error: { code: 'network_error', message: `Request failed (${response.status})` } }))
    throw new APIError(response.status, body.error)
  }
  if (response.status === 204) return undefined as T
  return response.json() as Promise<T>
}

export const upload = <T>(path: string, data: FormData) => api<T>(path, { method: 'POST', body: data })
