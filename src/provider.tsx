import { useMemo, type ReactNode } from "react"

import type { CheckoutAppearance } from "./appearance"
import { MessagesContext } from "./i18n/context"
import {
  createTranslator,
  resolveMessages,
  type BillingUiMessageBundle,
  type BillingUiTranslate,
} from "./i18n/messages"
import { UiContext, type Navigate } from "./scope-context"

export interface BillingUiProviderProps {
  appearance?: CheckoutAppearance
  /** Locale bundle(s) layered over English; later entries win. */
  messages?: BillingUiMessageBundle | readonly BillingUiMessageBundle[]
  /** Host translation hook, consulted before the bundles. */
  t?: BillingUiTranslate
  /** BCP 47 tag for dates and money; defaults to the browser's. */
  locale?: string
  /** Host router for in-app links (e.g. the plans page). */
  navigate?: Navigate
  children?: ReactNode
}

/** Renders no DOM; surfaces create their own styling roots. */
export function BillingUiProvider({
  appearance,
  messages,
  t,
  locale,
  navigate,
  children,
}: BillingUiProviderProps) {
  const translator = useMemo(
    () => createTranslator(resolveMessages(messages), t),
    [messages, t]
  )
  const ui = useMemo(
    () => ({ appearance, locale, navigate }),
    [appearance, locale, navigate]
  )
  return (
    <UiContext.Provider value={ui}>
      <MessagesContext.Provider value={translator}>
        {children}
      </MessagesContext.Provider>
    </UiContext.Provider>
  )
}
