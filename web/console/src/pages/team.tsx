import { MembersAdmin, type AdminNavigation } from "@openrails/auth-ui/admin"
import { useStepUpGuard } from "@openrails/auth-ui"
import type { AuthClient } from "@openrails/auth-ui/client"

// The operators: root's people. Where email is not configured an invitation
// is not sent, and the page shows its link to hand over privately: whoever
// uses it first gets its role.
export function TeamPage({
  auth,
  baseUrl,
  navigation,
}: {
  auth: AuthClient
  baseUrl: string
  navigation: AdminNavigation
}) {
  const guard = useStepUpGuard()
  return (
    <section className="console-section">
      <h1>Team</h1>
      <MembersAdmin
        auth={auth}
        baseUrl={baseUrl}
        navigation={navigation}
        guard={guard}
        group="root"
        basePath="/admin/team"
      />
    </section>
  )
}
