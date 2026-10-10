import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react"

import type { CheckoutAppearance } from "../appearance"
import type { BillingClient } from "../client/client"
import { MessagesContext } from "../i18n/context"
import {
  createTranslator,
  resolveMessages,
  type BillingUiMessageBundle,
  type BillingUiTranslate,
} from "../i18n/messages"
import { UiContext, type Navigate } from "../scope-context"
import { createConfigStore } from "./config"
import { BillingContext, type BillingChange } from "./context"

export interface BillingProviderProps {
  /**
   * The customer's client: hooks and the account panels read it, and a
   * checkout saves a new card to the customer first. Leave it out where no
   * customer is signed in, such as the shared payment page.
   */
  client?: BillingClient
  /** Fires after each successful mutation: the host's cache-invalidation hook. */
  onChange?: (change: BillingChange) => void
  appearance?: CheckoutAppearance
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
          <ClientProvider client={client} onChange={onChange}>
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
  onChange,
  children,
}: {
  client: BillingClient
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
  const config = useMemo(() => createConfigStore(client), [client])
  const value = useMemo(
    () => ({ client, version, notify, refresh, config }),
    [client, version, notify, refresh, config]
  )
  return (
    <BillingContext.Provider value={value}>{children}</BillingContext.Provider>
  )
}
