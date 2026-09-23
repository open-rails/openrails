import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react"

import type { BillingClient } from "../client/client"
import { BillingContext, type BillingChange } from "./context"

export interface BillingProviderProps {
  client: BillingClient
  /** Fires after each successful mutation: the host's cache-invalidation hook. */
  onChange?: (change: BillingChange) => void
  /**
   * Cancel and resume are queued server-side (202); hooks re-read the
   * subscription until the change shows. Default 1000 ms x 10.
   */
  settle?: { intervalMs?: number; attempts?: number }
  children?: ReactNode
}

export function BillingProvider({
  client,
  onChange,
  settle,
  children,
}: BillingProviderProps) {
  const [version, setVersion] = useState(0)
  const onChangeRef = useRef(onChange)
  useEffect(() => {
    onChangeRef.current = onChange
  })
  const notify = useCallback((change: BillingChange) => {
    setVersion((v) => v + 1)
    onChangeRef.current?.(change)
  }, [])
  const intervalMs = settle?.intervalMs ?? 1000
  const attempts = settle?.attempts ?? 10
  const value = useMemo(
    () => ({ client, version, notify, settle: { intervalMs, attempts } }),
    [client, version, notify, intervalMs, attempts]
  )
  return (
    <BillingContext.Provider value={value}>{children}</BillingContext.Provider>
  )
}
