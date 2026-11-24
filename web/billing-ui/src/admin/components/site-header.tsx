import { AdminMessage } from "#orck/admin/ui-context"
import * as React from "react"
import { Link } from "#orck/admin/router"

import { NotificationBell } from "#orck/admin/components/notification-bell"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "#orck/admin/components/ui/breadcrumb"
import { Separator } from "#orck/admin/components/ui/separator"
import { SidebarTrigger } from "#orck/admin/components/ui/sidebar"
import { useAuth } from "#orck/admin/lib/auth"
import { useMerchantConfig } from "#orck/admin/lib/capabilities"

export interface Crumb {
  label: string
  /** A link when the step is somewhere you can go; plain text otherwise. */
  to?: string
}

export function SiteHeader({ trail }: { trail: Crumb[] }) {
  const { activeMerchant } = useAuth()
  // Findings belong to merchant configuration.
  const merchantConfig = useMerchantConfig()
  return (
    <header className="flex h-14 shrink-0 items-center gap-2 border-b px-4">
      <SidebarTrigger className="-ml-1" />
      <Separator orientation="vertical" className="mr-2 h-4" />
      <Breadcrumb>
        <BreadcrumbList>
          {trail.length === 0 ? (
            <BreadcrumbItem>
              <BreadcrumbPage>
                <AdminMessage id={"Dashboard"} />
              </BreadcrumbPage>
            </BreadcrumbItem>
          ) : (
            <>
              <BreadcrumbItem>
                <BreadcrumbLink
                  render={
                    <Link to="/">
                      <AdminMessage id={"Dashboard"} />
                    </Link>
                  }
                />
              </BreadcrumbItem>
              {trail.map((crumb, index) => (
                <React.Fragment key={crumb.label}>
                  <BreadcrumbSeparator />
                  <BreadcrumbItem>
                    {crumb.to && index < trail.length - 1 ? (
                      <BreadcrumbLink
                        render={
                          <Link to={crumb.to}>
                            <AdminMessage id={crumb.label} />
                          </Link>
                        }
                      />
                    ) : (
                      <BreadcrumbPage>
                        <AdminMessage id={crumb.label} />
                      </BreadcrumbPage>
                    )}
                  </BreadcrumbItem>
                </React.Fragment>
              ))}
            </>
          )}
        </BreadcrumbList>
      </Breadcrumb>
      <div className="ml-auto flex items-center gap-2">
        {activeMerchant && merchantConfig && <NotificationBell />}
      </div>
    </header>
  )
}
