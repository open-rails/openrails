import { createRoot } from "react-dom/client"
import { BrowserRouter, useLocation, useNavigate } from "react-router-dom"
import { createAuthClient } from "@openrails/auth-ui/client"
import { AuthProvider } from "@openrails/auth-ui/react"
import { BillingAdmin } from "../../../dist/admin.js"
import "../../../dist/admin.css"

const params = new URLSearchParams(window.location.hash.slice(1))
// Test-only bootstrap: keep the synthetic session through URL navigation and reload.
const token = params.get("token") ?? sessionStorage.getItem("admin-test-token") ?? ""
if (token) sessionStorage.setItem("admin-test-token", token)
const requestedTheme = params.get("theme")
const theme = requestedTheme === "dark" || requestedTheme === "auto" ? requestedTheme : "light"
const historyOnly = window.location.pathname.startsWith("/admin/history")
const auth = createAuthClient({ baseUrl: "/auth/v1", sessionHint: false })
await auth.completeSignIn(async () => ({
  status: "complete",
  token_set: { access_token: token, token_type: "Bearer", expires_in: 3600 },
}))

export function Host() {
  const location = useLocation(),
    navigate = useNavigate()

  return (
    <>
      <header
        data-testid="host-header"
        style={{
          padding: 12,
          background: "rgb(240, 230, 210)",
          color: "rgb(30, 20, 10)",
        }}
      >
        Host application
      </header>
      <BillingAdmin
        baseUrl="/billing/v1"
        auth={auth}

        basePath={historyOnly ? "/admin/history" : "/admin/billing"}
        navigation={
          historyOnly
            ? undefined
            : { pathname: location.pathname, search: location.search, navigate }
        }
        appearance={{ theme }}
        settingsTabs={[
          {
            value: "host",
            title: "Host settings",
            element: <p>Host-owned settings</p>,
          },
        ]}
        customerPanels={(customer) => (
          <p data-testid="host-customer">Host user {customer.id}</p>
        )}
        userHref={(id) => `/host/users/${id}`}
      />
    </>
  )
}
createRoot(document.getElementById("root")!).render(
  <AuthProvider client={auth}>
    <BrowserRouter>
      <Host />
    </BrowserRouter>
  </AuthProvider>
)
