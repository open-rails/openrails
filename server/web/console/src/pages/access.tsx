import {
  ApiKeysAdmin,
  MembersAdmin,
  RemoteApplicationsAdmin,
  RolesAdmin,
  type GroupAdminAccess,
  type AdminNavigation,
} from "@openrails/auth-ui/admin"
import { useStepUpGuard } from "@openrails/auth-ui"
import { billingPermissionHelp } from "@openrails/billing-ui/admin"
import { Link, Navigate, Route, Routes } from "react-router-dom"
import type { AuthClient } from "@openrails/auth-ui/client"

export function AccessPage({
  auth,
  baseUrl,
  navigation,
  group,
  access,
}: {
  auth: AuthClient
  baseUrl: string
  navigation: AdminNavigation
  group: string
  access: GroupAdminAccess
}) {
  const guard = useStepUpGuard()
  if (access.status === "loading")
    return <p role="status">Loading access permissions…</p>
  if (access.status === "error")
    return (
      <div role="alert">
        Access permissions could not be loaded.{" "}
        <button onClick={access.reload}>Try again</button>
      </div>
    )
  const sections = [
    {
      key: "members",
      label: "People",
      visible: access.members.read || access.invite,
    },
    { key: "roles", label: "Roles", visible: access.roles.read },
    { key: "keys", label: "API keys", visible: access.credentials.read },
    {
      key: "applications",
      label: "Signing keys",
      visible: access.credentials.read,
    },
  ].filter((section) => section.visible)
  if (!sections.length)
    return (
      <p>
        You do not have access to this group's people, roles, or credentials.
      </p>
    )
  const props = { auth, baseUrl, navigation, group, guard }
  return (
    <section className="console-section">
      <h1>Access</h1>
      <nav aria-label="Access sections" className="console-tabs">
        {sections.map((section) => (
          <Link key={section.key} to={`/access/${section.key}`}>
            {section.label}
          </Link>
        ))}
      </nav>
      <Routes>
        <Route index element={<Navigate to={sections[0].key} replace />} />
        <Route
          path="members/*"
          element={
            sections.some((s) => s.key === "members") ? (
              <MembersAdmin {...props} basePath="/admin/access/members" />
            ) : (
              <p>No access to people.</p>
            )
          }
        />
        <Route
          path="roles/*"
          element={
            access.roles.read ? (
              <RolesAdmin
                {...props}
                basePath="/admin/access/roles"
                permissionHelp={billingPermissionHelp}
              />
            ) : (
              <p>No access to roles.</p>
            )
          }
        />
        <Route
          path="keys/*"
          element={
            access.credentials.read ? (
              <ApiKeysAdmin {...props} basePath="/admin/access/keys" />
            ) : (
              <p>No access to API keys.</p>
            )
          }
        />
        <Route
          path="applications/*"
          element={
            access.credentials.read ? (
              <RemoteApplicationsAdmin
                {...props}
                basePath="/admin/access/applications"
                scope="openrails:self"
              />
            ) : (
              <p>No access to signing keys.</p>
            )
          }
        />
        <Route path="*" element={<p>That access page does not exist.</p>} />
      </Routes>
    </section>
  )
}
