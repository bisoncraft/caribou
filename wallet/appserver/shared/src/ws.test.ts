import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import ws from './ws'

// FakeSocket stands in for window.WebSocket so the retry loop can be driven
// deterministically. close() fires onclose synchronously, which is what the
// browser does for a refused upgrade.
class FakeSocket {
  static instances: FakeSocket[] = []
  onclose: ((evt: any) => void) | null = null
  onopen: (() => void) | null = null
  onmessage: ((evt: any) => void) | null = null
  onerror: ((evt: any) => void) | null = null
  url: string

  constructor (url: string) {
    this.url = url
    FakeSocket.instances.push(this)
  }

  close () {
    if (this.onclose) this.onclose({ code: 1006 })
  }

  send () { /* not exercised here */ }
}

const noop = () => { /* no reload in tests */ }

describe('MessageSocket lifecycle', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    FakeSocket.instances = []
    ;(window as any).log = () => { /* silence ws logging */ }
    ;(window as any).WebSocket = FakeSocket
    ;(window as any).WebSocket.OPEN = 1
    // The socket is a module singleton; drop any link a previous test left.
    ws.disconnect()
    ws.handlers = {}
  })

  afterEach(() => {
    ws.disconnect()
    vi.useRealTimers()
  })

  it('retries when the link drops while a session is live', () => {
    ws.connect('ws://x/ws', noop)
    expect(FakeSocket.instances).toHaveLength(1)

    // Server drops the connection; the client should come back on its own.
    FakeSocket.instances[0].close()
    vi.advanceTimersByTime(11000)
    expect(FakeSocket.instances.length).toBeGreaterThan(1)
  })

  // Without this, a pre-login connect attempt leaves a retry loop running that
  // lands after login, counts as a reconnect, and reloads the page out from
  // under the user.
  it('stops retrying after disconnect', () => {
    ws.connect('ws://x/ws', noop)
    expect(FakeSocket.instances).toHaveLength(1)

    ws.disconnect()
    vi.advanceTimersByTime(60000)
    expect(FakeSocket.instances).toHaveLength(1)
  })

  // syncSocket calls connect on every app-state refresh, so a second call must
  // not stack a parallel link and retry loop on top of the live one.
  it('is idempotent while a link is live', () => {
    ws.connect('ws://x/ws', noop)
    ws.connect('ws://x/ws', noop)
    expect(FakeSocket.instances).toHaveLength(1)
  })

  it('is idempotent while a retry is pending', () => {
    ws.connect('ws://x/ws', noop)
    FakeSocket.instances[0].close() // schedules a retry
    ws.connect('ws://x/ws', noop)
    expect(FakeSocket.instances).toHaveLength(1)
  })

  // Handlers are registered once at startup, so a disconnect/reconnect cycle
  // across a logout must not silently stop delivering notifications.
  it('keeps route handlers registered across a reconnect', () => {
    const seen: any[] = []
    ws.registerRoute('notify', (payload: any) => seen.push(payload))

    ws.connect('ws://x/ws', noop)
    ws.disconnect()
    ws.connect('ws://x/ws', noop)

    const live = FakeSocket.instances[FakeSocket.instances.length - 1]
    live.onmessage?.({ data: JSON.stringify({ route: 'notify', payload: { id: 1 } }) } as any)
    expect(seen).toEqual([{ id: 1 }])
  })
})
