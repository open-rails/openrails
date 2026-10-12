// @vitest-environment jsdom
// The console follows the <base href> the server rewrites to its mount: router
// basename, bootstrap URL and alert links all derive from it.
import { afterEach, expect, it } from "vitest"

import { bootstrapURL, normalizeLink, routerBasename } from "@/lib/mount"

afterEach(() => document.querySelector("base")?.remove())

function mountAt(href: string) {
  const base = document.createElement("base")
  base.href = href
  document.head.prepend(base)
}

it("derives the mount from the document's base", () => {
  mountAt("/billing/admin/")
  expect(document.baseURI).toBe(`${location.origin}/billing/admin/`)
  expect(routerBasename()).toBe("/billing/admin")
  expect(bootstrapURL()).toBe(`${location.origin}/billing/admin/config.json`)
  expect(normalizeLink("/billing/admin/ops?finding=1")).toEqual({
    path: "/ops?finding=1",
  })
})

it("serves the default /admin mount", () => {
  const base = "https://console.example/admin/"
  expect(routerBasename(base)).toBe("/admin")
  expect(bootstrapURL(base)).toBe("https://console.example/admin/config.json")
})

it("maps alert links into the router", () => {
  const base = "https://host.example/billing/admin/"
  for (const [link, want] of [
    ["/ops?finding=1", { path: "/ops?finding=1" }],
    ["/billing/admin/ops", { path: "/ops" }],
    ["/billing/admin", { path: "/" }],
    ["/billing/administrator", { path: "/billing/administrator" }],
    ["ops", { path: "/ops" }],
    ["https://elsewhere.example/x", { href: "https://elsewhere.example/x" }],
    [null, {}],
  ] as const) {
    expect(normalizeLink(link, base)).toEqual(want)
  }
})
