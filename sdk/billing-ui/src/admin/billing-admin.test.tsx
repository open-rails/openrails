import { MemoryRouter } from "#orck/admin/test/router"
import { QueryClient } from "@tanstack/react-query"
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react"
import { useLocation, useNavigate } from "react-router-dom"
import { afterEach, expect, it } from "vitest"
import { BillingAdmin } from "./billing-admin"
import type { BillingAdminAuth } from "./types"

const access = {
  admin: "read",
  catalog: false,
  merchant_config: false,
  metrics: false,
  scope_id: "same-merchant",
  assistants: {
    catalog_copilot: false,
    catalog_drafting: false,
  },
}
function transport(name: string) {
  const requests: string[] = []
  const auth: BillingAdminAuth = {
    async authFetch(url) {
      requests.push(url)
      if (url.endsWith("/admin/access")) return Response.json(access)
      if (url.endsWith("/config"))
        return Response.json({
          capabilities: { features: {} },
          currencies: [],
          rails: [],
        })
      if (new URL(url, "http://local").pathname.endsWith("/admin/customers"))
        return Response.json({
          data: [
            {
              id: name.toLowerCase().replaceAll(" ", "-"),
              contact: { name },
              balances: [],
              created_at: "2026-10-11T00:00:00Z",
              last_seen_at: "2026-10-11T00:00:00Z",
            },
          ],
          next_cursor: null,
        })
      return Response.json({ data: [], next_cursor: null })
    },
  }
  return { auth, requests }
}
const cache = () =>
  new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
afterEach(() => window.history.replaceState(null, "", "/"))

it("isolates two authenticated instances even when their host shares one QueryClient", async () => {
  const first = transport("Customer A"),
    second = transport("Customer B"),
    queryClient = cache()
  const view = render(
    <>
      <section data-testid="first">
        <BillingAdmin
          baseUrl="/billing/v1"
          auth={first.auth}
          queryClient={queryClient}
          merchant="same-merchant"
          basePath="/first"
          navigation={{
            pathname: "/first/customers",
            search: "",
            navigate() {},
          }}
        />
      </section>
      <section data-testid="second">
        <BillingAdmin
          baseUrl="/billing/v1"
          auth={second.auth}
          queryClient={queryClient}
          merchant="same-merchant"
          basePath="/second"
          navigation={{
            pathname: "/second/customers",
            search: "",
            navigate() {},
          }}
        />
      </section>
    </>
  )
  expect(
    await within(screen.getByTestId("first")).findByText("Customer A")
  ).toBeVisible()
  expect(
    await within(screen.getByTestId("second")).findByText("Customer B")
  ).toBeVisible()
  expect(
    within(screen.getByTestId("first")).queryByText("Customer B")
  ).toBeNull()
  expect(
    within(screen.getByTestId("second")).queryByText("Customer A")
  ).toBeNull()
  expect(first.requests.some((url) => url.includes("/admin/customers"))).toBe(
    true
  )
  expect(second.requests.some((url) => url.includes("/admin/customers"))).toBe(
    true
  )
  view.unmount()
  queryClient.clear()
})

it("shares location with its host router, including filters and back navigation", async () => {
  const { auth } = transport("Customer routed")
  function Host() {
    const location = useLocation(),
      navigate = useNavigate()
    return (
      <>
        <output data-testid="host-location">
          {location.pathname}
          {location.search}
        </output>
        <button onClick={() => navigate(-1)}>Host back</button>
        <BillingAdmin
          baseUrl="/billing/v1"
          auth={auth}
          basePath="/host/billing"
          navigation={{
            pathname: location.pathname,
            search: location.search,
            navigate,
          }}
        />
      </>
    )
  }
  render(
    <MemoryRouter initialEntries={["/host/billing/customers?search=needle"]}>
      <Host />
    </MemoryRouter>
  )
  expect(await screen.findByText("Customer routed")).toBeVisible()
  fireEvent.click(screen.getByRole("link", { name: "Subscriptions" }))
  await waitFor(() =>
    expect(screen.getByTestId("host-location").textContent).toBe(
      "/host/billing/subscriptions"
    )
  )
  fireEvent.click(screen.getByRole("button", { name: "Host back" }))
  await waitFor(() =>
    expect(screen.getByTestId("host-location").textContent).toBe(
      "/host/billing/customers?search=needle"
    )
  )
  expect(
    screen.getByPlaceholderText("Search email, username, name or id…")
  ).toHaveValue("needle")
})

it("uses browser history without a router adapter and applies host messages", async () => {
  const { auth } = transport("Customer history")
  window.history.replaceState(null, "", "/history/customers")
  render(
    <BillingAdmin
      baseUrl="/billing/v1"
      auth={auth}
      basePath="/history"
      language="de"
      messages={{
        Customers: "Kunden",
        "Search email, username, name or id…": "Kunden suchen",
      }}
    />
  )
  expect(await screen.findByText("Customer history")).toBeVisible()
  expect(screen.getByRole("heading", { name: "Kunden" })).toBeVisible()
  expect(screen.getByPlaceholderText("Kunden suchen")).toBeVisible()
  fireEvent.click(screen.getByRole("link", { name: "Subscriptions" }))
  await waitFor(() =>
    expect(window.location.pathname).toBe("/history/subscriptions")
  )
  window.history.back()
  await waitFor(() =>
    expect(window.location.pathname).toBe("/history/customers")
  )
  expect(await screen.findByRole("heading", { name: "Kunden" })).toBeVisible()
})

it("discards cached records when the host replaces the authenticated transport", async () => {
  const first = transport("Old identity"),
    second = transport("New identity")
  const navigation = {
    pathname: "/billing/customers",
    search: "",
    navigate() {},
  }
  const queryClient = cache()
  const view = render(
    <BillingAdmin
      baseUrl="/billing/v1"
      auth={first.auth}
      queryClient={queryClient}
      basePath="/billing"
      navigation={navigation}
    />
  )
  expect(await screen.findByText("Old identity")).toBeVisible()
  view.rerender(
    <BillingAdmin
      baseUrl="/billing/v1"
      auth={second.auth}
      queryClient={queryClient}
      basePath="/billing"
      navigation={navigation}
    />
  )
  expect(await screen.findByText("New identity")).toBeVisible()
  expect(screen.queryByText("Old identity")).toBeNull()
})

it("refuses to display customer data when the supplied transport names another merchant", async () => {
  const { auth, requests } = transport("Wrong merchant data")
  render(
    <BillingAdmin
      baseUrl="/billing/v1"
      auth={auth}
      merchant="requested-merchant"
      basePath="/billing"
      navigation={{ pathname: "/billing/customers", search: "", navigate() {} }}
    />
  )
  expect(await screen.findByRole("alert")).toHaveTextContent(
    "bound to another merchant"
  )
  expect(requests.every((url) => url.endsWith("/admin/access"))).toBe(true)
  expect(screen.queryByText("Wrong merchant data")).toBeNull()
})
