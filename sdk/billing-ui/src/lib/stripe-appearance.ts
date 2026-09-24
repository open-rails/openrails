// Stripe Elements appearance from the page's resolved theme tokens, so the
// Payment Element matches our inputs (height, radius, type, colors).
import type { Appearance } from "@stripe/stripe-js"

import { toPlainColor } from "#orck/lib/collect"

export function stripeAppearance(host: HTMLElement): Appearance {
  const style = window.getComputedStyle(host)
  const token = (name: string) =>
    toPlainColor(style.getPropertyValue(name).trim()) || undefined
  const dark = !!host.closest(".dark")
  return {
    theme: dark ? "night" : "stripe",
    variables: {
      colorPrimary: token("--primary"),
      colorBackground: token("--card"),
      colorText: token("--foreground"),
      colorTextSecondary: token("--muted-foreground"),
      colorDanger: token("--destructive"),
      fontFamily: style.fontFamily || undefined,
      fontSizeBase: "14px",
      borderRadius: "9px",
      spacingUnit: "3px",
    },
    rules: {
      ".Input": {
        padding: "9px 10px",
        boxShadow: "none",
        borderColor: token("--border") ?? "",
      },
      ".Input:focus": {
        borderColor: token("--ring") ?? "",
        boxShadow: "none",
      },
      ".Label": { fontSize: "13px", fontWeight: "500", marginBottom: "6px" },
      ".Error": { fontSize: "12.5px" },
    },
  }
}
