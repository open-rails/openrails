/* eslint-disable react-refresh/only-export-components */
import { useCallback, useEffect } from "react"
import { useHref, useLocation, useNavigate } from "react-router-dom"

const trimmed = (path: string) => path.replace(/\/+$/, "") || "/"

// useRedirect replaces the page it was rendered on. While a navigation
// renders, the router's location trails the address bar, so a redirect
// decided on the page the operator just left is dropped rather than
// replacing the one they opened.
export function useRedirect() {
  const navigate = useNavigate()
  const decided = new URL(useHref(useLocation()), window.location.origin)
  const path = trimmed(decided.pathname),
    search = decided.search
  return useCallback(
    (to: string) => {
      if (
        trimmed(window.location.pathname) === path &&
        window.location.search === search
      )
        void navigate(to, { replace: true })
    },
    [navigate, path, search]
  )
}

export function Redirect({ to }: { to: string }) {
  const redirect = useRedirect()
  useEffect(() => redirect(to), [redirect, to])
  return null
}
