import { useSyncExternalStore } from "react"
import type { CheckoutTheme } from "../appearance"

const media = () => window.matchMedia?.("(prefers-color-scheme: dark)")
const subscribe = (notify: () => void) => {
  const query = media()
  query?.addEventListener("change", notify)
  return () => query?.removeEventListener("change", notify)
}
const snapshot = () => (media()?.matches ? "dark" : "light")
const serverSnapshot = () => "light" as const
const unchanged = () => () => {}

export function useAdminTheme(theme: CheckoutTheme = "inherit") {
  const system = useSyncExternalStore(
    theme === "auto" ? subscribe : unchanged,
    snapshot,
    serverSnapshot
  )
  return theme === "auto" ? system : theme
}
