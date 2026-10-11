// Roles are AuthKit's, in the two groups staff hold their permissions in:
// the root group, whose roles apply at every merchant, and the merchant's own
// (GET /v1/admin/access's scope_id), with the roles its persona declares and
// the custom ones the merchant defines. The console calls AuthKit's group
// routes at auth_base_url.
import {
  mutationOptions,
  queryOptions,
  useQuery,
  type QueryClient,
} from "@tanstack/react-query"
import type {
  APIKey,
  APIKeyCreated,
  GroupMember,
  GroupRole,
  ListPage as AuthKitPage,
  PermissionSet,
} from "@openrails/auth-ui/client"

import { authKit, collectCursorPages, getBootstrap } from "@/lib/api/client"
import {
  OPENRAILS_ROUTES,
  type OpenRailsRoute,
} from "@/lib/api/generated/routes"
import { useAccess } from "@/lib/capabilities"
import { merchantQueryKeys, queryKeys } from "@/lib/queries"

// ROOT_SCOPE is AuthKit's name for its root group.
export const ROOT_SCOPE = "root"

// What the caller may do with the scope's roles, from their permissions
// there: the Roles tab shows with roles:read and edits with roles:manage.
export interface RolesAccess {
  scope: string
  // everyMerchant: the root group, whose staff act at every merchant.
  everyMerchant: boolean
  persona: string
  held: ReadonlySet<string>
  read: boolean
  manage: boolean
  membersRead: boolean
  membersManage: boolean
  keysRead: boolean
  keysManage: boolean
}

export function rolesAccess(
  scope: string,
  me: PermissionSet
): RolesAccess | null {
  const persona = me.role?.split(":")[0]
  if (!persona) return null
  const held = new Set(me.permissions)
  const holds = (perm: string) => held.has(`${persona}:${perm}`)
  return {
    scope,
    everyMerchant: scope === ROOT_SCOPE,
    persona,
    held,
    read: holds("roles:read"),
    manage: holds("roles:read") && holds("roles:manage"),
    membersRead: holds("members:read"),
    membersManage: holds("members:read") && holds("members:manage"),
    keysRead: holds("credentials:read"),
    keysManage: holds("credentials:manage"),
  }
}

// useRolesAccess is the scopes whose roles the caller may read: the root
// group's (all-merchants staff), then the merchant's own, which a host that
// grants only root roles has none of. Empty until known, and where there are
// no roles to manage: no AuthKit API here, or no role in either.
export function useRolesAccess(): RolesAccess[] {
  const scope = useAccess()?.scope_id ?? ""
  const enabled = scope !== "" && authKitServed()
  const root = useQuery({ ...rolesQueries.me(ROOT_SCOPE), enabled })
  const own = useQuery({ ...rolesQueries.me(scope), enabled })
  if (!enabled) return []
  return [
    root.data ? rolesAccess(ROOT_SCOPE, root.data) : null,
    own.data ? rolesAccess(scope, own.data) : null,
  ].filter((access): access is RolesAccess => access?.read ?? false)
}

const authKitServed = () => {
  try {
    return getBootstrap().auth_base_url !== ""
  } catch {
    return false
  }
}

const group = (scope: string) => `/groups/${encodeURIComponent(scope)}`

const rolesKey = (scope: string) =>
  [...queryKeys.merchant(), "roles", scope] as const

const collect = <T>(path: string, query: Record<string, string> = {}) =>
  collectCursorPages<T>((cursor) =>
    authKit<AuthKitPage<T>>(path, { query: { ...query, cursor } })
  )

export const rolesQueries = {
  me: (scope: string) =>
    queryOptions({
      queryKey: [...rolesKey(scope), "me"],
      queryFn: ({ signal }) =>
        authKit<PermissionSet>("/me/permissions", {
          query: { group_id: scope },
          signal,
        }),
      staleTime: 60_000,
      retry: false,
    }),
  list: (scope: string) =>
    queryOptions({
      queryKey: [...rolesKey(scope), "list"],
      queryFn: ({ signal }) =>
        authKit<AuthKitPage<GroupRole>>(`${group(scope)}/roles`, {
          signal,
        }).then((page) => page.data),
      meta: { errorAction: "Load roles" },
    }),
  members: (scope: string) =>
    queryOptions({
      queryKey: [...rolesKey(scope), "members"],
      queryFn: () =>
        collect<GroupMember>(`${group(scope)}/members`, {
          kind: "user",
          expand: "user",
        }),
      meta: { errorAction: "Load the team" },
    }),
  apiKeys: (scope: string) =>
    queryOptions({
      queryKey: [...rolesKey(scope), "api-keys"],
      queryFn: () => collect<APIKey>(`${group(scope)}/api-keys`),
      meta: { errorAction: "Load API keys" },
    }),
}

// Every change refreshes the scope's roles, team, keys and the caller's own
// permissions: an edit reaches every holder, the caller among them.
const changed = (queryClient: QueryClient, scope: string) => {
  const key = [...merchantQueryKeys().merchant(), "roles", scope]
  return () => queryClient.invalidateQueries({ queryKey: key })
}

const rolePath = (scope: string, role: string) =>
  `${group(scope)}/roles/${encodeURIComponent(role)}`

export const rolesMutations = {
  create: (queryClient: QueryClient, scope: string) =>
    mutationOptions({
      mutationFn: (body: { name: string; permissions: string[] }) =>
        authKit<GroupRole>(`${group(scope)}/roles`, { method: "POST", body }),
      onSuccess: changed(queryClient, scope),
    }),
  update: (queryClient: QueryClient, scope: string) =>
    mutationOptions({
      mutationFn: ({
        role,
        permissions,
      }: {
        role: string
        permissions: string[]
      }) =>
        authKit<GroupRole>(rolePath(scope, role), {
          method: "PATCH",
          body: { permissions },
        }),
      onSuccess: changed(queryClient, scope),
    }),
  remove: (queryClient: QueryClient, scope: string) =>
    mutationOptions({
      mutationFn: (role: string) =>
        authKit<void>(rolePath(scope, role), { method: "DELETE" }),
      onSuccess: changed(queryClient, scope),
    }),
  setMemberRole: (queryClient: QueryClient, scope: string) =>
    mutationOptions({
      mutationFn: ({ userId, role }: { userId: string; role: string }) =>
        authKit<GroupMember>(
          `${group(scope)}/members/users/${encodeURIComponent(userId)}`,
          { method: "PUT", body: { role } }
        ),
      onSuccess: changed(queryClient, scope),
    }),
  createAPIKey: (queryClient: QueryClient, scope: string) =>
    mutationOptions({
      mutationFn: (body: { name: string; role: string }) =>
        authKit<APIKeyCreated>(`${group(scope)}/api-keys`, {
          method: "POST",
          body,
        }),
      onSuccess: changed(queryClient, scope),
    }),
}

// The permissions a role may hold, by what they open. Programs are the
// /v1/app routes an API key calls; staff, the console's areas; access,
// AuthKit's own over the team, keys and roles.
export type PermissionSection = "programs" | "staff" | "operator" | "metrics" | "access"

export interface PermissionInfo {
  label: string
  section: PermissionSection
}

// OpenRails' own, by their merchant: names; a root: twin reads the same.
const ROUTE_PERMISSIONS: Record<string, PermissionInfo> = {
  "merchant:entitlements:read": {
    label: "Check entitlements",
    section: "programs",
  },
  "merchant:catalog:read": { label: "Read offers", section: "programs" },
  "merchant:orders:manage": {
    label: "Create and cancel orders",
    section: "programs",
  },
  "merchant:usage:manage": {
    label: "Admit and record usage (charges customers)",
    section: "programs",
  },
  "merchant:events:read": {
    label: "Read and acknowledge host events",
    section: "programs",
  },
  "merchant:customers:read": {
    label: "Find a customer by openrails.dev account",
    section: "programs",
  },
  "merchant:customer:read": {
    label: "Read customers, subscriptions and payments",
    section: "staff",
  },
  "merchant:customer:manage": {
    label: "Refunds, cancellations and credits",
    section: "staff",
  },
  "merchant:catalog:manage": { label: "Manage the catalog", section: "operator" },
  "merchant:config:manage": {
    label: "PSPs, findings and merchant settings",
    section: "operator",
  },
  "merchant:metrics:read": {
    label: "Metrics and the dashboard",
    section: "metrics",
  },
}

// AuthKit's built-ins, the same on every persona, by resource and action.
const ACCESS_PERMISSIONS: Record<string, PermissionInfo> = {
  "members:read": { label: "See the team", section: "access" },
  "members:manage": {
    label: "Invite, change and remove team members",
    section: "access",
  },
  "credentials:read": { label: "See API keys", section: "access" },
  "credentials:manage": {
    label: "Create and revoke API keys",
    section: "access",
  },
  "roles:read": { label: "See what roles grant", section: "access" },
  "roles:manage": {
    label: "Create, change and delete roles",
    section: "access",
  },
  "directory:read": {
    label: "Read the trusted issuers' user directory",
    section: "access",
  },
  "directory:manage": {
    label: "Provision the trusted issuers' users",
    section: "access",
  },
}

export const SECTIONS: {
  id: PermissionSection | "other"
  title: string
  description: string
}[] = [
  {
    id: "programs",
    title: "Programs (/v1/app)",
    description:
      "What an API key's program may call. A storefront, for example, needs only entitlements and offers.",
  },
  {
    id: "staff",
    title: "Customer support",
    description: "Read and manage customers and their billing.",
  },
  {
    id: "operator",
    title: "Operator capabilities",
    description: "Manage the catalog, payment providers, findings and merchant configuration.",
  },
  {
    id: "metrics",
    title: "Business metrics",
    description: "Read reports and dashboard metrics.",
  },
  {
    id: "access",
    title: "Team and access",
    description: "Who holds what: the team, API keys and roles.",
  },
  {
    id: "other",
    title: "Other",
    description: "Permissions this console does not describe.",
  },
]

// permissionInfo describes perm of persona; null for another persona's or
// one the console does not know.
export function permissionInfo(
  perm: string,
  persona: string
): PermissionInfo | null {
  const prefix = `${persona}:`
  if (!perm.startsWith(prefix)) return null
  const rest = perm.slice(prefix.length)
  return (
    ROUTE_PERMISSIONS[`merchant:${rest}`] ?? ACCESS_PERMISSIONS[rest] ?? null
  )
}

// appRoutes are the /v1/app routes a permission opens, by either of a
// route's names.
export function appRoutes(perm: string): OpenRailsRoute[] {
  return OPENRAILS_ROUTES.filter(
    (route) =>
      route.group === "app" &&
      (route.permission?.merchant === perm || route.permission?.root === perm)
  )
}

// grantLabel names one grant of a role: a permission, or a pattern.
export function grantLabel(grant: string, persona: string): string {
  if (grant === `${persona}:*`) return "Everything"
  const info = permissionInfo(grant, persona)
  if (info) return info.label
  const rest = grant.startsWith(`${persona}:`)
    ? grant.slice(persona.length + 1)
    : grant
  return rest.endsWith(":*") ? `All of ${rest.slice(0, -2)}` : rest
}

// roleTitle is a role's name as people read it: a custom role's own name,
// a declared role's capitalized.
export function roleTitle(role: string): string {
  const name = role.slice(role.indexOf(":") + 1)
  if (name.startsWith("custom-")) return name.slice("custom-".length)
  return name.charAt(0).toUpperCase() + name.slice(1)
}

// catalogOf is every permission a role of the scope may hold: what its
// owner role covers.
export function catalogOf(roles: GroupRole[], persona: string): string[] {
  const owner = roles.find((role) => role.name === `${persona}:owner`)
  const perms = owner
    ? owner.permissions
    : roles.flatMap((role) => role.permissions)
  return [...new Set(perms)].sort()
}

// A custom role's name: lowercase letters, digits and dashes, at most 64.
export const ROLE_NAME = /^[a-z][a-z0-9-]{0,63}$/
