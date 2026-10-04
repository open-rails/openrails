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
  children?: ReactNode
}

export function BillingProvider({
  client,
  onChange,
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
  const refresh = useCallback(() => setVersion((v) => v + 1), [])
  const value = useMemo(
    () => ({ client, version, notify, refresh }),
    [client, version, notify, refresh]
  )
  return (
    <BillingContext.Provider value={value}>{children}</BillingContext.Provider>
  )
}
