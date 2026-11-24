import { useEffect, useMemo, useSyncExternalStore } from "react"
import type { z } from "zod"

import { localError, toBillingError, type BillingError } from "../client/errors"
import type {
  CurrencyScales,
  CustomerConfig,
  PublicConfig,
} from "../client/types"
import { useBillingContext } from "./context"

export interface ConfigState<T = PublicConfig> {
  config: T | null
  loading: boolean
  error: BillingError | null
  refetch: () => void
}

interface Snapshot<T> {
  config: T | null
  loading: boolean
  error: BillingError | null
}

/** One shared copy of a configuration read per client, fetched on first use. */
export interface ConfigStore<T = PublicConfig> {
  subscribe(listener: () => void): () => void
  snapshot(): Snapshot<T>
  /** Fetches the document unless it is loaded or loading. */
  ensure(): void
  refetch(): void
}

/**
 * The store of `read`'s document. `supplied` is the host's copy: the store
 * starts with it and never reads to load; a refetch (a retry) still reads.
 */
export function createConfigStore<T>(
  read: () => Promise<T>,
  supplied?: Snapshot<T>
): ConfigStore<T> {
  let snapshot: Snapshot<T> = supplied ?? {
    config: null,
    loading: true,
    error: null,
  }
  let started = supplied !== undefined
  let latest = 0
  const listeners = new Set<() => void>()
  const set = (next: Snapshot<T>) => {
    snapshot = next
    for (const listener of listeners) listener()
  }
  const load = () => {
    started = true
    const request = ++latest
    if (!snapshot.loading) set({ ...snapshot, loading: true })
    read().then(
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
    // A read that failed with nothing loaded is tried again by the next
    // component: the customer may have signed in since.
    ensure() {
      if (supplied) return
      if (!started || (snapshot.error && !snapshot.config && !snapshot.loading))
        load()
    },
    refetch: load,
  }
}

/** The host's copy of a document, read as `schema` reads the server's. */
export function suppliedConfig<T>(
  schema: z.ZodType<T>,
  name: string,
  value: unknown
): Snapshot<T> {
  const parsed = schema.safeParse(value)
  if (parsed.success)
    return { config: parsed.data, loading: false, error: null }
  const issue = parsed.error.issues[0]
  return {
    config: null,
    loading: false,
    error: localError(
      "invalid_config",
      `${name}: ${issue?.path.join(".")} ${issue?.message}`
    ),
  }
}

function useStore<T>(store: ConfigStore<T>): ConfigState<T> {
  const snapshot = useSyncExternalStore(
    store.subscribe,
    store.snapshot,
    store.snapshot
  )
  useEffect(() => store.ensure(), [store])
  return { ...snapshot, refetch: store.refetch }
}

/**
 * The deployment's configuration (`GET /config`), fetched once per
 * `BillingProvider` unless its `config` supplied it, and shared by every
 * component that reads it.
 */
export function useConfig(): ConfigState {
  return useStore(useBillingContext().config)
}

/**
 * The customer's merchant (`GET /me/config`): its payment setup and how it
 * presents itself, fetched once per `BillingProvider` for the signed-in
 * customer unless its `customerConfig` supplied it, and shared by every
 * component that reads it.
 */
export function useCustomerConfig(): ConfigState<CustomerConfig> {
  return useStore(useBillingContext().customerConfig)
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
