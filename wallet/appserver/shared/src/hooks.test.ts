import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { useFlash } from './hooks'

beforeEach(() => {
  vi.useFakeTimers()
})

afterEach(() => {
  vi.useRealTimers()
})

describe('useFlash', () => {
  it('turns on, then resets after the duration', () => {
    const { result } = renderHook(() => useFlash(1500))
    expect(result.current[0]).toBe(false)

    act(() => result.current[1]())
    expect(result.current[0]).toBe(true)

    act(() => { vi.advanceTimersByTime(1500) })
    expect(result.current[0]).toBe(false)
  })

  it('restarts the window on a re-flash', () => {
    const { result } = renderHook(() => useFlash(1500))
    act(() => result.current[1]())
    act(() => { vi.advanceTimersByTime(1000) })
    act(() => result.current[1]())
    act(() => { vi.advanceTimersByTime(1000) })
    expect(result.current[0]).toBe(true)
    act(() => { vi.advanceTimersByTime(500) })
    expect(result.current[0]).toBe(false)
  })

  it('clears immediately and cancels the pending reset', () => {
    const { result } = renderHook(() => useFlash(1500))
    act(() => result.current[1]())
    act(() => result.current[2]())
    expect(result.current[0]).toBe(false)
    // No stray timer left behind.
    expect(vi.getTimerCount()).toBe(0)
  })

  it('cancels the pending reset on unmount', () => {
    const { result, unmount } = renderHook(() => useFlash(1500))
    act(() => result.current[1]())
    unmount()
    expect(vi.getTimerCount()).toBe(0)
  })
})
