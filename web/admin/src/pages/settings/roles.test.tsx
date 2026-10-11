// @vitest-environment jsdom
// The Roles tab against the real api client, auth-ui session and query cache:
// AuthKit's group routes for the root group and the scope the access read
// names.
import { QueryClientProvider } from "@tanstack/react-query"
import { MemoryRouter } from "react-router-dom"
import { afterEach, expect, it, vi } from "vitest"
import type { GroupRole, PermissionSet } from "@openrails/auth-ui/client"

import { useRolesAccess } from "@/lib/roles"
import {
  calls,
  client,
  FULL_ACCESS,
  selectMerchant,
  server,
  type Recorded,
  type Reply,
} from "@/test/harness"
import { act, browserEnvironment, choose, mount, unmount } from "@/test/mount"
import { RolesTab } from "./roles"

afterEach(async () => {
  await unmount()
  vi.restoreAllMocks()
})

const SCOPE = "11111111-2222-4333-8444-555555555555"
const GROUP = `/groups/${SCOPE}`
const ROOT = "/groups/root"
const WHEN = "2026-10-10T00:00:00Z"

const CATALOG = [
  "merchant:billing:read",
  "merchant:billing:manage",
  "merchant:catalog:manage",
  "merchant:catalog:read",
  "merchant:config:manage",
  "merchant:costs:manage",
  "merchant:credentials:manage",
  "merchant:credentials:read",
  "merchant:entitlements:read",
  "merchant:events:read",
  "merchant:members:manage",
  "merchant:members:read",
  "merchant:metrics:read",
  "merchant:orders:manage",
  "merchant:roles:manage",
  "merchant:roles:read",
  "merchant:usage:manage",
]

const role = (name: string, grants: string[], custom = false): GroupRole => ({
  name,
  grants,
  permissions: grants.includes("merchant:*") ? CATALOG : grants,
  custom,
  requires_mfa: false,
  created_at: custom ? WHEN : null,
  updated_at: custom ? WHEN : null,
})

const OWNER = role("merchant:owner", ["merchant:*"])
const VIEWER = role("merchant:viewer", ["merchant:billing:read"])
const STOREFRONT = role(
  "merchant:custom-storefront",
  ["merchant:catalog:read", "merchant:entitlements:read"],
  true
)

const me = (role: string, permissions: string[]): PermissionSet => ({
  group_id: SCOPE,
  role,
  permissions,
})

// nobody is AuthKit's answer for a group the caller holds no role in, or one
// that does not exist.
const nobody = { role: null, permissions: [] }

const ROOT_CATALOG = [
  "root:billing:read",
  "root:billing:manage",
  "root:members:read",
  "root:roles:read",
]
const ROOT_SUPPORT = role("root:support", [
  "root:billing:read",
  "root:billing:manage",
])
const ROOT_ADMIN = role("root:staff-admin", ROOT_CATALOG)
const ROOT_MEMBERS = [
  {
    subject: { kind: "user", id: "u-root-1" },
    role: "root:support",
    user: { id: "u-root-1", username: "rosa" },
  },
  {
    subject: { kind: "user", id: "u-root-2" },
    role: "root:support",
    user: { id: "u-root-2", username: "remy" },
  },
  {
    subject: { kind: "user", id: "u-root-3" },
    role: "root:staff-admin",
    user: { id: "u-root-3", username: "ada" },
  },
]

const page = <T,>(data: T[]) => ({ data, next_cursor: null, total: null })

function Roles() {
  const access = useRolesAccess()
  if (access.length === 0) return <p>No roles here</p>
  return access.map((each) => <RolesTab key={each.scope} access={each} />)
}

// open mounts the tab. routes' "GET /me/permissions" is the caller in the
// merchant's group; root is the caller in the root group, nobody unless given.
async function open(routes: Record<string, Reply>, root: unknown = nobody) {
  browserEnvironment()
  const { "GET /me/permissions": mine = nobody, ...rest } = routes
  const requests = await server({
    "GET /admin/access": { ...FULL_ACCESS, scope_id: SCOPE },
    "GET /me/permissions": (request: Recorded) =>
      request.query === "group_id=root" ? root : mine,
    [`GET ${GROUP}/members`]: page([
      {
        subject: { kind: "user", id: "u-owner" },
        role: "merchant:owner",
        user: { id: "u-owner", username: "olive" },
      },
      {
        subject: { kind: "user", id: "u-dev" },
        role: "merchant:viewer",
        user: { id: "u-dev", username: "devon" },
      },
    ]),
    [`GET ${GROUP}/api-keys`]: page([]),
    ...rest,
  })
  selectMerchant("acme")
  await mount(
    <QueryClientProvider client={client()}>
      <MemoryRouter>
        <Roles />
      </MemoryRouter>
    </QueryClientProvider>
  )
  await settle()
  return requests
}

const settle = () =>
  act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0))
  })

const buttons = () =>
  [...document.querySelectorAll("button")].map((node) =>
    node.textContent!.trim()
  )

async function press(label: string) {
  const found = [...document.querySelectorAll("button")].find(
    (node) => node.textContent!.trim() === label
  )
  expect(found, `button ${label}`).toBeDefined()
  expect(found!.disabled).toBe(false)
  await act(async () => found!.click())
  await settle()
}

const box = (perm: string) => {
  const found = document.querySelector<HTMLInputElement>(
    `#perm-${perm.replaceAll(":", "-")}`
  )
  expect(found, perm).not.toBeNull()
  return found!
}

const type = (selector: string, value: string) =>
  act(async () => {
    const input = document.querySelector<HTMLInputElement>(selector)!
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value"
    )!.set!.call(input, value)
    input.dispatchEvent(new Event("input", { bubbles: true }))
  })

const writes = (requests: Recorded[]) =>
  requests
    .filter((request) => request.method !== "GET")
    .map((request) => [`${request.method} ${request.path}`, request.body])

it("an owner composes a program role from the /v1/app permissions", async () => {
  let roles = [OWNER, VIEWER]
  const requests = await open({
    "GET /me/permissions": me("merchant:owner", CATALOG),
    [`GET ${GROUP}/roles`]: () => page(roles),
    [`POST ${GROUP}/roles`]: () => {
      roles = [...roles, STOREFRONT]
      return STOREFRONT
    },
  })
  expect(
    requests
      .filter((r) => r.path === "/me/permissions")
      .map((r) => r.query)
      .sort()
  ).toEqual([`group_id=${SCOPE}`, "group_id=root"])
  const text = document.body.textContent!
  for (const shown of ["Owner", "Everything", "Viewer", "1 member · 0 keys"])
    expect(text).toContain(shown)
  expect(buttons()).toEqual(["New role", "Assign", "Assign"])

  await press("New role")
  const dialog = document.body.textContent!
  expect(dialog).toContain("Programs (/v1/app)")
  expect(dialog).toContain("POST /v1/app/entitlements/check")
  expect(dialog).toContain("GET /v1/app/catalog/products")
  expect(dialog).toContain("POST /v1/app/usage-events")
  await type("#role-name", "storefront")
  await act(async () => box("merchant:entitlements:read").click())
  await act(async () => box("merchant:catalog:read").click())
  await press("Create role")

  expect(writes(requests)).toEqual([
    [
      `POST ${GROUP}/roles`,
      {
        name: "storefront",
        permissions: ["merchant:entitlements:read", "merchant:catalog:read"],
      },
    ],
  ])
  expect(calls(requests).at(-1)).toMatch(/^GET /)
  expect(document.body.textContent).toContain("storefront")
  expect(buttons()).toContain("Edit")
  expect(buttons()).toContain("Delete")
})

it("an edit replaces what the role grants; a permission the editor lacks is not offered", async () => {
  const held = CATALOG.filter((perm) => perm !== "merchant:usage:manage")
  const requests = await open({
    "GET /me/permissions": me("merchant:custom-admin", held),
    [`GET ${GROUP}/roles`]: page([OWNER, STOREFRONT]),
    [`PATCH ${GROUP}/roles/merchant%3Acustom-storefront`]: STOREFRONT,
  })
  await press("Edit")
  expect(box("merchant:entitlements:read").checked).toBe(true)
  expect(box("merchant:usage:manage").disabled).toBe(true)
  await act(async () => box("merchant:entitlements:read").click())
  await act(async () => box("merchant:events:read").click())
  await press("Save role")
  expect(writes(requests)).toEqual([
    [
      `PATCH ${GROUP}/roles/merchant%3Acustom-storefront`,
      { permissions: ["merchant:catalog:read", "merchant:events:read"] },
    ],
  ])
})

it("a role is given to a teammate or to a new API key, whose secret shows once", async () => {
  const requests = await open({
    "GET /me/permissions": me("merchant:owner", CATALOG),
    [`GET ${GROUP}/roles`]: page([OWNER, VIEWER, STOREFRONT]),
    [`PUT ${GROUP}/members/users/u-dev`]: {
      subject: { kind: "user", id: "u-dev" },
      role: STOREFRONT.name,
      user: null,
    },
    [`POST ${GROUP}/api-keys`]: {
      api_key: { id: "key_1", name: "storefront-prod", role: STOREFRONT.name },
      secret: "openrails_st_secret",
    },
  })
  const assign = [...document.querySelectorAll("button")].filter(
    (node) => node.textContent === "Assign"
  )
  await act(async () => assign[2].click())
  await settle()
  expect(document.body.textContent).toContain("Assign storefront")

  await choose("devon")
  await press("Give role")
  await type("#assign-key", "storefront-prod")
  await press("Create key")
  expect(writes(requests)).toEqual([
    [`PUT ${GROUP}/members/users/u-dev`, { role: STOREFRONT.name }],
    [`POST ${GROUP}/api-keys`, { name: "storefront-prod", role: STOREFRONT.name }],
  ])
  expect(
    document.querySelector<HTMLInputElement>('[aria-label="API key secret"]')
      ?.value
  ).toBe("openrails_st_secret")
})

it("roles show read-only without roles:manage, and not at all without roles:read", async () => {
  await open({
    "GET /me/permissions": me("merchant:custom-auditor", [
      "merchant:roles:read",
    ]),
    [`GET ${GROUP}/roles`]: page([OWNER, STOREFRONT]),
  })
  expect(document.body.textContent).toContain("storefront")
  expect(buttons()).toEqual([])
  await unmount()

  await open({
    "GET /me/permissions": me("merchant:viewer", ["merchant:billing:read"]),
  })
  expect(document.body.textContent).toContain("No roles here")
})

// A host that grants only root roles has no group for its merchant: AuthKit
// answers nobody there, and the tab shows the root group's roles with their
// holders, as all-merchants staff. With both, both show; with the merchant's
// alone, only its own.
it("the root group's holders show as all-merchants staff, beside the merchant's own", async () => {
  const rootRoutes = {
    [`GET ${ROOT}/roles`]: page([ROOT_SUPPORT, ROOT_ADMIN]),
    [`GET ${ROOT}/members`]: page(ROOT_MEMBERS),
  }
  const admin = {
    group_id: "g-root",
    role: "root:staff-admin",
    permissions: ROOT_CATALOG,
  }

  // Root roles only: no group for the merchant.
  let requests = await open(rootRoutes, admin)
  let text = document.body.textContent!
  for (const shown of [
    "All-merchants staff",
    "support",
    "2 members",
    "staff-admin",
    "1 member",
  ])
    expect(text).toContain(shown)
  expect(text).not.toContain("No roles here")
  expect(calls(requests)).not.toContain(`GET ${GROUP}/roles`)
  await unmount()

  // Both: the root group's first, then the merchant's own.
  await open(
    {
      ...rootRoutes,
      "GET /me/permissions": me("merchant:owner", CATALOG),
      [`GET ${GROUP}/roles`]: page([OWNER, VIEWER]),
    },
    admin
  )
  text = document.body.textContent!
  expect(text.indexOf("All-merchants staff")).toBeGreaterThanOrEqual(0)
  expect(text.indexOf("Viewer")).toBeGreaterThan(
    text.indexOf("All-merchants staff")
  )
  for (const shown of ["2 members", "Owner", "1 member · 0 keys"])
    expect(text).toContain(shown)
  await unmount()

  // The merchant's group alone: no root section, and the root group is not read.
  requests = await open({
    "GET /me/permissions": me("merchant:owner", CATALOG),
    [`GET ${GROUP}/roles`]: page([OWNER, VIEWER]),
  })
  text = document.body.textContent!
  expect(text).toContain("Owner")
  expect(text).not.toContain("All-merchants staff")
  expect(calls(requests)).not.toContain(`GET ${ROOT}/roles`)
})
