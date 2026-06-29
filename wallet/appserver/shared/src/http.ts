// requestJSON encodes the object and sends the JSON to the specified address.
// On a 200 response with a JSON body, the parsed object is returned. The Go
// API contract is `{ok: bool, msg?: string, code?: number}` for endpoints that
// can report application-level failure; endpoints that always succeed (e.g.
// `/api/buildinfo`) omit `ok` entirely. To keep both shapes ergonomic for
// callers, we only default `ok` to true when the server did not include it —
// we never overwrite an explicit `ok: false`.
//
// On a non-200 response or a fetch/JSON-parse failure, returns
// `{ok: false, errMsg: <best-effort message>}`.
export async function requestJSON (method: string, addr: string, reqBody?: any): Promise<any> {
  let response: Response
  try {
    response = await window.fetch(addr, {
      method,
      headers: new window.Headers({ 'content-type': 'application/json' }),
      body: reqBody
    })
  } catch (e: any) {
    return { ok: false, errMsg: e?.message || 'network error' }
  }
  if (response.status !== 200) {
    let errMsg = ''
    try { errMsg = await response.text() } catch { /* ignore */ }
    return { ok: false, errMsg: errMsg || `HTTP ${response.status}` }
  }
  try {
    const obj = await response.json()
    if (typeof obj.ok !== 'boolean') obj.ok = true
    return obj
  } catch (e: any) {
    return { ok: false, errMsg: e?.message || 'invalid JSON response' }
  }
}

// postJSON sends a POST request with JSON-formatted data and returns the
// response.
export async function postJSON (addr: string, data?: any) {
  return requestJSON('POST', addr, JSON.stringify(data))
}

// getJSON sends a GET request and returns the response.
export async function getJSON (addr: string): Promise<any> {
  return requestJSON('GET', addr)
}
