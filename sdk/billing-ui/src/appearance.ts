// Integrator theming. The component scopes the standard shadcn token set
// under its root, so `theme` flips the whole palette and `variables` override
// individual tokens without the integrator touching our CSS.
import type { CSSProperties } from "react"

// `inherit` uses the host page's shadcn tokens (`--background`, ...) and its
// `.dark` class instead of the bundled zinc palette.
export type BillingTheme = "light" | "dark" | "auto" | "inherit"

// The tokens integrators may override. Values are raw CSS color/length
// strings; they land as custom properties on the purchase root.
export interface BillingVariables {
  background?: string
  foreground?: string
  card?: string
  mutedForeground?: string
  faintForeground?: string
  border?: string
  input?: string
  primary?: string
  primaryForeground?: string
  destructive?: string
  ring?: string
  radius?: string
  fontFamily?: string
}

export interface BillingAppearance {
  theme?: BillingTheme
  variables?: BillingVariables
}

const VARIABLE_TO_CSS: Record<keyof BillingVariables, string> = {
  background: "--background",
  foreground: "--foreground",
  card: "--card",
  mutedForeground: "--muted-foreground",
  faintForeground: "--orck-faint",
  border: "--border",
  input: "--input",
  primary: "--primary",
  primaryForeground: "--primary-foreground",
  destructive: "--destructive",
  ring: "--ring",
  radius: "--radius",
  fontFamily: "--orck-font",
}

export function appearanceStyle(
  appearance: BillingAppearance | undefined,
  namespace: "orck" | "orba" = "orck"
): CSSProperties | undefined {
  const variables = appearance?.variables
  if (!variables) return undefined
  const style: Record<string, string> = {}
  for (const [key, value] of Object.entries(variables)) {
    if (!value) continue
    style[
      VARIABLE_TO_CSS[key as keyof BillingVariables].replace(
        "--orck-",
        `--${namespace}-`
      )
    ] = value
  }
  return style as CSSProperties
}

export function appearanceTheme(
  appearance: BillingAppearance | undefined
): BillingTheme {
  return appearance?.theme ?? "auto"
}
