import { describe, expect, it } from "vitest"
import { Home01Icon } from "@hugeicons/core-free-icons"

import {
  buildNav,
  extensionRoute,
  newMerchantPath,
  userMenuItems,
  validateExtensions,
} from "@/extensions/registry"
import type { ConsoleContext, ConsoleExtension } from "@/extensions/types"

const page = () => Promise.resolve({ Component: () => null })
const owner = { id: "m1", slug: "acme", role: "owner" }
const viewer = { id: "m1", slug: "acme", role: "viewer" }

const contextFor =
  (ctx: Partial<ConsoleContext>, config: Record<string, unknown> = {}) =>
  (id?: string): ConsoleContext => ({
    me: { id: "u1", email: "ada@example.com" },
    merchants: ctx.activeMerchant ? [ctx.activeMerchant] : [],
    ...ctx,
    config: id === undefined ? undefined : config[id],
  })

const hosted: ConsoleExtension = {
  id: "hosted",
  newMerchantPath: "/merchants/new",
  routes: [
    {
      path: "/account",
      scope: "user",
      lazy: page,
      trail: [{ label: "Account" }],
    },
    { path: "/merchants/new", scope: "user", lazy: page },
    { path: "/plan", scope: "merchant", lazy: page },
    { path: "/platform/merchants/:slug", scope: "user", lazy: page },
  ],
  nav: [
    { title: "Overview", path: "/account", scope: "user", icon: Home01Icon },
    {
      title: "Plan",
      path: "/plan",
      scope: "merchant",
      group: "Setup",
      roles: ["owner"],
    },
    {
      title: "Platform",
      path: "/platform",
      scope: "user",
      order: 50,
      visible: (ctx) =>
        (ctx.config as { operator?: boolean })?.operator === true,
    },
  ],
  userMenu: [{ title: "Account settings", path: "/account/settings" }],
}

const labels = (groups: ReturnType<typeof buildNav>) =>
  groups.map((group) => [group.label, group.items.map((item) => item.title)])

describe("console navigation", () => {
  it("is the console's own Billing group on a standalone deployment", () => {
    expect(labels(buildNav([], contextFor({ activeMerchant: owner })))).toEqual(
      [
        [
          "Billing",
          [
            "Dashboard",
            "Customers",
            "Subscriptions",
            "Payments",
            "Invoices",
            "Catalog",
            "Ops",
            "Settings",
          ],
        ],
      ]
    )
  })

  it("shows no merchant pages until a merchant is selected", () => {
    expect(buildNav([], contextFor({}))).toEqual([])
    expect(labels(buildNav([hosted], contextFor({})))).toEqual([
      ["Account", ["Overview"]],
    ])
  })

  it("adds a host's groups after Billing and honours roles and visibility", () => {
    const ownerNav = buildNav(
      [hosted],
      contextFor({ activeMerchant: owner }, { hosted: { operator: true } })
    )
    expect(labels(ownerNav).map(([label]) => label)).toEqual([
      "Billing",
      "Setup",
      "Account",
    ])
    expect(labels(ownerNav)[2]).toEqual(["Account", ["Platform", "Overview"]])

    const viewerNav = buildNav([hosted], contextFor({ activeMerchant: viewer }))
    expect(labels(viewerNav).map(([label]) => label)).toEqual([
      "Billing",
      "Account",
    ])
    expect(labels(viewerNav)[1]).toEqual(["Account", ["Overview"]])
  })

  it("routes extension pages, params included", () => {
    expect(extensionRoute([hosted], "/account")?.scope).toBe("user")
    expect(extensionRoute([hosted], "/plan")?.scope).toBe("merchant")
    expect(extensionRoute([hosted], "/platform/merchants/acme")?.path).toBe(
      "/platform/merchants/:slug"
    )
    expect(extensionRoute([hosted], "/customers")).toBeUndefined()
  })

  it("offers the host's New merchant page and account-menu entries", () => {
    expect(newMerchantPath([])).toBeUndefined()
    expect(newMerchantPath([hosted])).toBe("/merchants/new")
    expect(
      userMenuItems([hosted], contextFor({})).map((item) => item.title)
    ).toEqual(["Account settings"])
  })
})

describe("extension validation", () => {
  const refuses = (extensions: ConsoleExtension[], message: RegExp) =>
    expect(() => validateExtensions(extensions)).toThrow(message)

  it("accepts a well-formed set", () => {
    expect(() => validateExtensions([hosted])).not.toThrow()
  })

  it("refuses a path the console already routes", () => {
    refuses(
      [{ id: "x", routes: [{ path: "/settings", scope: "user", lazy: page }] }],
      /already routed/
    )
  })

  it("refuses two extensions claiming one path or id", () => {
    const route = { path: "/a", scope: "user" as const, lazy: page }
    refuses(
      [
        { id: "x", routes: [route] },
        { id: "y", routes: [route] },
      ],
      /already routed/
    )
    refuses([{ id: "x" }, { id: "x" }], /must be unique/)
  })

  it("refuses malformed ids and paths", () => {
    refuses([{ id: "Not OK" }], /must be unique lowercase/)
    refuses(
      [{ id: "x", nav: [{ title: "T", path: "account", scope: "user" }] }],
      /console path/
    )
    refuses(
      [{ id: "x", userMenu: [{ title: "T", path: "//evil.example" }] }],
      /console path/
    )
  })

  it("allows one New merchant page", () => {
    refuses(
      [
        { id: "x", newMerchantPath: "/new" },
        { id: "y", newMerchantPath: "/other" },
      ],
      /only one extension/
    )
  })
})
