import { createContext, useContext, type CSSProperties } from "react"

import {
  appearanceStyle,
  appearanceTheme,
  type CheckoutAppearance,
  type CheckoutTheme,
} from "./appearance"

export type Navigate = (to: string) => void

export interface UiSettings {
  appearance?: CheckoutAppearance
  locale?: string
  navigate?: Navigate
}

export const UiContext = createContext<UiSettings>({})

export const useUiSettings = (): UiSettings => useContext(UiContext)

export interface ScopeProps {
  className: string
  "data-orck-theme": CheckoutTheme
  style?: CSSProperties
}

/** Props that make an element a styling root; portals carry them too. */
export function useScopeProps(appearance?: CheckoutAppearance): ScopeProps {
  const settings = useUiSettings()
  const resolved = appearance ?? settings.appearance
  return {
    className: "orck",
    "data-orck-theme": appearanceTheme(resolved),
    style: appearanceStyle(resolved),
  }
}
