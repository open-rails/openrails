import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  useSyncExternalStore,
  type ComponentProps,
  type ReactNode,
} from "react"
import type { BillingAdminNavigation } from "./types"

type NavigateOptions = { replace?: boolean; state?: unknown }
type Location = { pathname: string; search: string; state: unknown }
interface RouterValue {
  location: Location
  href(to: string): string
  navigate(to: string | number, options?: NavigateOptions): void
  params: Record<string, string>
}
const Context = createContext<RouterValue | null>(null)
const subscribe = (notify: () => void) => {
  window.addEventListener("popstate", notify)
  return () => window.removeEventListener("popstate", notify)
}
const snapshot = () => window.location.pathname + window.location.search

export function AdminRouter({
  basePath = "/",
  navigation,
  children,
}: {
  basePath?: string
  navigation?: BillingAdminNavigation
  children: ReactNode
}) {
  const current = useSyncExternalStore(subscribe, snapshot, () => basePath)
  const base = basePath.replace(/\/$/, "")
  const fallback = new URL(current, "http://billing.local")
  const pathname = navigation?.pathname ?? fallback.pathname
  const search = navigation?.search ?? fallback.search
  const relative =
    pathname === base
      ? "/"
      : pathname.startsWith(base + "/")
        ? pathname.slice(base.length)
        : "/"
  const [states, setStates] = useState(() => new Map<string, unknown>())
  const href = useCallback(
    (to: string) => {
      if (/^(?:[a-z][a-z\d+.-]*:|\/\/)/i.test(to)) return to
      const resolved = new URL(to, "http://billing.local" + relative + search)
      return base + resolved.pathname + resolved.search + resolved.hash
    },
    [base, relative, search]
  )
  const navigate = useCallback(
    (to: string | number, options: NavigateOptions = {}) => {
      if (typeof to === "number") {
        window.history.go(to)
        return
      }
      const target = href(to)
      if (/^(?:[a-z][a-z\d+.-]*:|\/\/)/i.test(target)) {
        window.location.assign(target)
        return
      }
      setStates((previous) => new Map(previous).set(target, options.state))
      if (navigation) navigation.navigate(target, { replace: options.replace })
      else {
        window.history[options.replace ? "replaceState" : "pushState"](
          options.state ?? null,
          "",
          target
        )
        window.dispatchEvent(new PopStateEvent("popstate"))
      }
    },
    [href, navigation]
  )
  const value = useMemo<RouterValue>(
    () => ({
      location: {
        pathname: relative,
        search,
        state:
          states.get(pathname + search) ??
          (!navigation ? window.history.state : null),
      },
      href,
      navigate,
      params: {},
    }),
    [relative, pathname, search, href, navigate, navigation, states]
  )
  return <Context.Provider value={value}>{children}</Context.Provider>
}

function useRouter() {
  const router = useContext(Context)
  if (!router) throw new Error("BillingAdmin router is missing")
  return router
}
export const useLocation = () => useRouter().location
export const useNavigate = () => useRouter().navigate
export const useParams = <T extends string = string>() =>
  useRouter().params as Partial<Record<T, string>>
export function useSearchParams() {
  const { location, navigate } = useRouter()
  const params = useMemo(
    () => new URLSearchParams(location.search),
    [location.search]
  )
  const set = (
    value: URLSearchParams | Record<string, string>,
    options?: NavigateOptions
  ) => {
    const query = new URLSearchParams(value).toString()
    navigate(location.pathname + (query ? "?" + query : ""), options)
  }
  return [params, set] as const
}

export function Link({
  to,
  onClick,
  ...props
}: Omit<ComponentProps<"a">, "href"> & { to: string }) {
  const { href, navigate } = useRouter()
  return (
    <a
      {...props}
      href={href(to)}
      onClick={(event) => {
        onClick?.(event)
        if (
          event.defaultPrevented ||
          event.button !== 0 ||
          event.metaKey ||
          event.ctrlKey ||
          event.shiftKey ||
          event.altKey ||
          props.target ||
          props.download
        )
          return
        event.preventDefault()
        navigate(to)
      }}
    />
  )
}

export function Navigate({ to, replace }: { to: string; replace?: boolean }) {
  const navigate = useNavigate()
  useEffect(() => navigate(to, { replace }), [navigate, to, replace])
  return null
}

export function RouteParams({
  params,
  children,
}: {
  params: Record<string, string>
  children: ReactNode
}) {
  const router = useRouter()
  return (
    <Context.Provider value={{ ...router, params }}>
      {children}
    </Context.Provider>
  )
}
