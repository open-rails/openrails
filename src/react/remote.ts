import { useCallback, useEffect, useRef, useState } from "react"

import { toBillingError, type BillingError } from "../client/errors"

export interface RemoteState<T> {
  data: T | null
  loading: boolean
  error: BillingError | null
  refetch: () => void
}

// Keyed fetch that keeps the previous data while a newer key loads, so a
// refetch after a mutation never flashes an empty list.
export function useRemote<T>(
  key: string,
  load: (signal: AbortSignal) => Promise<T>
): RemoteState<T> & { replace: (update: (data: T) => T) => void } {
  const [nonce, setNonce] = useState(0)
  const request = `${key}|${nonce}`
  const [state, setState] = useState<{
    request: string | null
    data: T | null
    error: BillingError | null
  }>({ request: null, data: null, error: null })
  const loadRef = useRef(load)
  useEffect(() => {
    loadRef.current = load
  })

  useEffect(() => {
    const ctl = new AbortController()
    loadRef.current(ctl.signal).then(
      (data) => setState({ request, data, error: null }),
      (err: unknown) => {
        if (!ctl.signal.aborted)
          setState((s) => ({
            request,
            data: s.data,
            error: toBillingError(err),
          }))
      }
    )
    return () => ctl.abort()
  }, [request])

  const refetch = useCallback(() => setNonce((n) => n + 1), [])
  const replace = useCallback(
    (update: (data: T) => T) =>
      setState((s) => (s.data === null ? s : { ...s, data: update(s.data) })),
    []
  )
  return {
    data: state.data,
    loading: state.request !== request,
    error: state.request === request ? state.error : null,
    refetch,
    replace,
  }
}

export const sleep = (ms: number) =>
  new Promise<void>((resolve) => setTimeout(resolve, ms))
