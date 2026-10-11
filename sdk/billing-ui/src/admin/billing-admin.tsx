import { AdminUIContext } from "./ui-context"
import {
  Component,
  useId,
  useMemo,
  useRef,
  useCallback,
  useLayoutEffect,
  useState,
  type ReactNode,
} from "react"
import { QueryClientProvider } from "@tanstack/react-query"
import { appearanceStyle } from "../appearance"
import { AdminContext, createAdminServices } from "./context"
import { AdminRouter, useLocation } from "./router"
import { AdminPages } from "./routes"
import { AppLayout } from "./layouts/app-layout"
import { ConfirmProvider } from "./components/confirm"
import { Toaster } from "./components/ui/sonner"
import { RouteError } from "./components/route-error"
import { createAdminQueryClient } from "./lib/query-client"
import type { BillingAdminMerchant, BillingAdminProps } from "./types"
import { useAdminTheme } from "./theme"

const merchantFrom = (
  merchant: string | BillingAdminMerchant
): BillingAdminMerchant =>
  typeof merchant === "string" ? { id: merchant, slug: merchant } : merchant

/** Staff billing pages inside the host's app, authentication, router, and theme. */
export function BillingAdmin(props: BillingAdminProps) {
  const [selected, setSelected] = useState<string>()
  const [connection, setConnection] = useState({
    auth: props.auth,
    baseUrl: props.baseUrl,
    generation: 0,
  })
  if (connection.auth !== props.auth || connection.baseUrl !== props.baseUrl)
    setConnection({
      auth: props.auth,
      baseUrl: props.baseUrl,
      generation: connection.generation + 1,
    })
  return <BillingAdminInstance key={connection.generation} {...props} selected={selected} onSelected={setSelected} />
}

function BillingAdminInstance({ selected, onSelected, ...props }: BillingAdminProps & { selected?: string; onSelected: (value: string) => void }) {
  const theme = useAdminTheme(props.appearance?.theme)
  const namespace = useId()
  const [portal, setPortal] = useState<HTMLDivElement | null>(null)
  const merchants = useMemo(
    () =>
      props.merchants ??
      (props.merchant
        ? [merchantFrom(props.merchant)]
        : [{ id: "", slug: "", display_name: "Billing" }]),
    [props.merchants, props.merchant]
  )
  const requested = props.merchant ? merchantFrom(props.merchant).id : selected
  const activeMerchant =
    merchants.find((m) => m.id === requested || m.slug === requested) ??
    merchants[0]
  const scope = `${props.baseUrl}:${activeMerchant?.id ?? ""}:${activeMerchant?.slug ?? ""}`
  const currentScope = useRef<string | undefined>(scope)
  useLayoutEffect(() => {
    currentScope.current = scope
    return () => {
      currentScope.current = undefined
    }
  }, [scope])
  const isCurrent = useCallback(() => currentScope.current === scope, [scope])
  const queryClient = useMemo(
    () => props.queryClient ?? createAdminQueryClient(namespace),
    [namespace, props.queryClient]
  )
  const services = useMemo(
    () =>
      // The factory captures this callback; only request-time checks read the ref.
      // eslint-disable-next-line react-hooks/refs
      createAdminServices({
        namespace,
        isCurrent,
        baseUrl: props.baseUrl,
        auth: props.auth,
        merchant: activeMerchant,
        exchange: props.exchange,
      }),
    [
      isCurrent,
      namespace,
      props.baseUrl,
      props.auth,
      props.exchange,
      activeMerchant,
    ]
  )
  const value = {
    props,
    services,
    merchants,
    activeMerchant,
    portal,
    selectMerchant(slug: string) {
      const merchant = merchants.find((m) => m.slug === slug || m.id === slug)
      if (!merchant) return
      onSelected(merchant.id)
      props.onMerchantChange?.(merchant)
    },
  }
  return (
    <div
      lang={props.language ?? "en"}
      className={["orba", props.className].filter(Boolean).join(" ")}
      data-orba-theme={theme}
      style={{ ...appearanceStyle(props.appearance, "orba"), ...props.style }}
    >
      <AdminUIContext.Provider value={{ namespace, messages: props.messages }}>
        <AdminContext.Provider value={value}>
          <QueryClientProvider client={queryClient}>
            <AdminRouter
              basePath={props.basePath}
              navigation={props.navigation}
            >
              <ConfirmProvider>
                <AppLayout>
                  <PageBoundary>
                    <AdminPages />
                  </PageBoundary>
                </AppLayout>
              </ConfirmProvider>
              <Toaster />
            </AdminRouter>
          </QueryClientProvider>
          <div ref={setPortal} />
        </AdminContext.Provider>
      </AdminUIContext.Provider>
    </div>
  )
}

class Boundary extends Component<{ children: ReactNode }, { error?: unknown }> {
  state: { error?: unknown } = {}
  static getDerivedStateFromError(error: unknown) {
    return { error }
  }
  render() {
    return this.state.error ? (
      <RouteError error={this.state.error} />
    ) : (
      this.props.children
    )
  }
}
function PageBoundary({ children }: { children: ReactNode }) {
  const location = useLocation()
  return <Boundary key={location.pathname}>{children}</Boundary>
}
