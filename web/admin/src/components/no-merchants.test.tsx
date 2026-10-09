// @vitest-environment jsdom
import { createElement } from "react"
import { renderToStaticMarkup } from "react-dom/server"
import { MemoryRouter } from "react-router-dom"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { beforeEach, describe, expect, it, vi } from "vitest"

import { NoMerchants } from "@/components/no-merchants"
import { ThemeProvider } from "@/components/theme-provider"
import { ConsoleExtensions } from "@/extensions/registry"
import type { ConsoleExtension } from "@/extensions/types"
import { AppLayout } from "@/layouts/app-layout"
import { render, server } from "@/test/harness"
import { browserEnvironment } from "@/test/mount"

const auth = vi.hoisted(() => ({
  value: {
    ready: true,
    signedIn: true,
    merchantsFailed: false,
    merchants: [] as unknown[],
    me: { email: "ada@example.com" },
    logout: () => Promise.resolve(),
  },
}))
vi.mock("@/lib/auth", () => ({ useAuth: () => auth.value }))

beforeEach(async () => {
  browserEnvironment()
  await server()
  auth.value = { ...auth.value, merchants: [], merchantsFailed: false }
})

const page = () => Promise.resolve({ Component: () => null })
const hosted: ConsoleExtension = {
  id: "hosted",
  newMerchantPath: "/merchants/new",
  routes: [
    { path: "/account", scope: "user", lazy: page },
    { path: "/merchants/new", scope: "user", lazy: page },
  ],
  nav: [
    { title: "Overview", path: "/account", scope: "user" },
    {
      title: "Operators only",
      path: "/platform",
      scope: "user",
      useVisible: () => false,
    },
  ],
}

// renderAt renders node at a console path with the given extensions.
const renderAt = (
  path: string,
  extensions: ConsoleExtension[],
  node: React.ReactNode
) =>
  renderToStaticMarkup(
    createElement(
      MemoryRouter,
      { initialEntries: [path] },
      createElement(
        QueryClientProvider,
        { client: new QueryClient() },
        createElement(ThemeProvider, {
          children: createElement(ConsoleExtensions, {
            extensions,
            config: {},
            children: node,
          }),
        })
      )
    )
  )

describe("a user with no merchants", () => {
  it("is pointed at an operator on a standalone deployment", () => {
    const html = render(<NoMerchants />)
    expect(html).toContain("You don&#x27;t have access to any merchants yet")
    expect(html).toContain("Ask an operator of this OpenRails deployment")
    expect(html).not.toContain("New merchant")
  })

  it("gets the host's New merchant action when an extension declares one", () => {
    const html = renderAt("/", [hosted], <NoMerchants />)
    expect(html).toContain("New merchant")
    expect(html).toContain("Create a merchant to start accepting payments")
    expect(html).not.toContain("Ask an operator")
  })

  it("sees the empty state instead of a merchant's dashboard", () => {
    expect(render(<AppLayout />)).toContain(
      "You don&#x27;t have access to any merchants yet"
    )
  })

  it("keeps a host's shell, with the empty state only on merchant pages", () => {
    const merchantPage = renderAt("/customers", [hosted], <AppLayout />)
    expect(merchantPage).toContain(
      "You don&#x27;t have access to any merchants yet"
    )
    expect(merchantPage).toContain("Overview")
    expect(merchantPage).not.toContain("Sign out")

    const userPage = renderAt("/account", [hosted], <AppLayout />)
    expect(userPage).not.toContain("any merchants yet")
    expect(userPage).toContain("Overview")
    expect(userPage).not.toContain("Operators only")
  })

  it("is told a failed load is a failure, not an empty list", () => {
    auth.value = { ...auth.value, merchantsFailed: true }
    const html = render(<AppLayout />)
    expect(html).toContain("Could not load your merchants.")
    expect(html).not.toContain("any merchants yet")
  })
})
