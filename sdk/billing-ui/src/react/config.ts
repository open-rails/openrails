import { useEffect, useMemo, useSyncExternalStore } from "react"

import type { BillingClient } from "../client/client"
import { toBillingError, type BillingError } from "../client/errors"
import type { CurrencyScales, PublicConfig } from "../client/types"
import { useBillingContext } from "./context"

export interface ConfigState {
  config: PublicConfig | null
  loading: boolean
  error: BillingError | null
  refetch: () => void
}

interface Snapshot {
  config: PublicConfig | null
  loading: boolean
  error: BillingError | null
}

/** One shared copy of `GET /config` per client, fetched on first use. */
export interface ConfigStore {
  subscribe(listener: () => void): () => void
  snapshot(): Snapshot
  /** Fetches the document unless it was already requested. */
  ensure(): void
  refetch(): void
}

export function createConfigStore(client: BillingClient): ConfigStore {
  let snapshot: Snapshot = { config: null, loading: true, error: null }
  let started = false
  let latest = 0
  const listeners = new Set<() => void>()
  const set = (next: Snapshot) => {
    snapshot = next
    for (const listener of listeners) listener()
  }
  const load = () => {
    started = true
    const request = ++latest
    if (!snapshot.loading) set({ ...snapshot, loading: true })
    client.getConfig().then(
      (config) => {
        if (request === latest) set({ config, loading: false, error: null })
      },
      (err: unknown) => {
        if (request === latest)
          set({
            config: snapshot.config,
            loading: false,
            error: toBillingError(err),
          })
      }
    )
  }
  return {
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    snapshot: () => snapshot,
    ensure() {
      if (!started) load()
    },
    refetch: load,
  }
}

/**
 * The deployment's public configuration (`GET /config`), fetched once per
 * `BillingProvider` and shared by every component that reads it.
 */
export function useConfig(): ConfigState {
  const { config: store } = useBillingContext()
  const snapshot = useSyncExternalStore(
    store.subscribe,
    store.snapshot,
    store.snapshot
  )
  useEffect(() => store.ensure(), [store])
  return { ...snapshot, refetch: store.refetch }
}

/**
 * Currency scales: the server's registry, under the client's pinned copy and
 * host overrides, which stand alone until the registry loads.
 */
export function useCurrencyScales(): CurrencyScales {
  const { client } = useBillingContext()
  const { config } = useConfig()
  return useMemo(() => {
    const served = Object.fromEntries(
      (config?.currencies ?? []).map((c) => [c.code.toUpperCase(), c.decimals])
    )
    return { ...served, ...client.currencies }
  }, [config, client])
}
