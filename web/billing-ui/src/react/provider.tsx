import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react"

import type { BillingAppearance } from "../appearance"
import type { BillingClient } from "../client/client"
import {
  customerConfigSchema,
  publicConfigSchema,
  type CustomerConfig,
  type PublicConfig,
} from "../client/types"
import { MessagesContext } from "../i18n/context"
import {
  createTranslator,
  resolveMessages,
  type BillingUiMessageBundle,
  type BillingUiTranslate,
} from "../i18n/messages"
import { UiContext, type Navigate } from "../scope-context"
import { createConfigStore, suppliedConfig } from "./config"
import { BillingContext, type BillingChange } from "./context"

export interface BillingProviderProps {
  /**
   * The customer's client: hooks and the account panels read it, and a
   * purchase saves a new card to the customer first. Leave it out where no
   * customer is signed in, such as the shared payment page.
   */
  client?: BillingClient
  /**
   * The deployment's configuration as `GET /config` answers it, from the
   * host's own page response (Go: `Engine.PublicConfig`, or a remote
   * `Client.GetPublicConfig`). Supplied, billing-ui does not request it.
   */
  config?: PublicConfig
  /**
   * The customer's merchant's configuration as `GET /me/config` answers it,
   * from the host's own page response (Go: `Engine.CustomerConfig`).
   * Supplied, billing-ui does not request it to load; a retry still reads it.
   */
  customerConfig?: CustomerConfig
  /** Fires after each successful mutation: the host's cache-invalidation hook. */
  onChange?: (change: BillingChange) => void
  appearance?: BillingAppearance
  /** Locale bundle(s) layered over English; later entries win. */
  messages?: BillingUiMessageBundle | readonly BillingUiMessageBundle[]
  /** Host translation hook, consulted before the bundles. */
  t?: BillingUiTranslate
  /** BCP 47 tag for dates, money and plurals; defaults to the browser's. */
  locale?: string
  /** Host router for in-app links (e.g. the plans page). */
  navigate?: Navigate
  children?: ReactNode
}

/**
 * The one billing-ui provider: the client, and the appearance and words of
 * the styled components. Renders no DOM; surfaces create their own styling
 * roots, and no stylesheet loads until a styled component is imported.
 */
export function BillingProvider({
  client,
  config,
  customerConfig,
  onChange,
  appearance,
  messages,
  t,
  locale,
  navigate,
  children,
}: BillingProviderProps) {
  const translator = useMemo(
    () => createTranslator(resolveMessages(messages), t, locale),
    [messages, t, locale]
  )
  const ui = useMemo(
    () => ({ appearance, locale, navigate }),
    [appearance, locale, navigate]
  )
  return (
    <UiContext.Provider value={ui}>
      <MessagesContext.Provider value={translator}>
        {client ? (
          <ClientProvider
            client={client}
            config={config}
            customerConfig={customerConfig}
            onChange={onChange}
          >
            {children}
          </ClientProvider>
        ) : (
          children
        )}
      </MessagesContext.Provider>
    </UiContext.Provider>
  )
}

function ClientProvider({
  client,
  config: hostConfig,
  customerConfig: hostCustomerConfig,
  onChange,
  children,
}: {
  client: BillingClient
  config?: PublicConfig
  customerConfig?: CustomerConfig
  onChange?: (change: BillingChange) => void
  children?: ReactNode
}) {
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
  const config = useMemo(
    () =>
      createConfigStore(
        () => client.getConfig(),
        hostConfig && suppliedConfig(publicConfigSchema, "config", hostConfig)
      ),
    [client, hostConfig]
  )
  const customerConfig = useMemo(
    () =>
      createConfigStore(
        () => client.getCustomerConfig(),
        hostCustomerConfig &&
          suppliedConfig(
            customerConfigSchema,
            "customerConfig",
            hostCustomerConfig
          )
      ),
    [client, hostCustomerConfig]
  )
  const value = useMemo(
    () => ({ client, version, notify, refresh, config, customerConfig }),
    [client, version, notify, refresh, config, customerConfig]
  )
  return (
    <BillingContext.Provider value={value}>{children}</BillingContext.Provider>
  )
}
