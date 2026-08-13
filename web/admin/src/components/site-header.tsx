import * as React from "react"
import { HugeiconsIcon } from "@hugeicons/react"
import {
  Logout01Icon,
  MoonIcon,
  Settings01Icon,
  Sun01Icon,
} from "@hugeicons/core-free-icons"
import { Link, useNavigate } from "react-router-dom"
import { useMutation } from "@tanstack/react-query"

import { NotificationBell } from "@/components/notification-bell"
import { useTheme } from "@/components/theme-provider"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Separator } from "@/components/ui/separator"
import { SidebarTrigger } from "@/components/ui/sidebar"
import { useAuth } from "@/lib/auth"
import { authMutations } from "@/lib/auth-mutations"

export interface Crumb {
  label: string
  /** A link when the step is somewhere you can go; plain text otherwise. */
  to?: string
}

export function SiteHeader({ trail }: { trail: Crumb[] }) {
  return (
    <header className="flex h-14 shrink-0 items-center gap-2 border-b px-4">
      <SidebarTrigger className="-ml-1" />
      <Separator orientation="vertical" className="mr-2 h-4" />
      <Breadcrumb>
        <BreadcrumbList>
          {trail.length === 0 ? (
            <BreadcrumbItem>
              <BreadcrumbPage>Dashboard</BreadcrumbPage>
            </BreadcrumbItem>
          ) : (
            <>
              <BreadcrumbItem>
                <BreadcrumbLink render={<Link to="/">Dashboard</Link>} />
              </BreadcrumbItem>
              {trail.map((crumb, index) => (
                <React.Fragment key={crumb.label}>
                  <BreadcrumbSeparator />
                  <BreadcrumbItem>
                    {crumb.to && index < trail.length - 1 ? (
                      <BreadcrumbLink
                        render={<Link to={crumb.to}>{crumb.label}</Link>}
                      />
                    ) : (
                      <BreadcrumbPage>{crumb.label}</BreadcrumbPage>
                    )}
                  </BreadcrumbItem>
                </React.Fragment>
              ))}
            </>
          )}
        </BreadcrumbList>
      </Breadcrumb>
      <div className="ml-auto flex items-center gap-2">
        <NotificationBell />
        <UserMenu />
      </div>
    </header>
  )
}

function UserMenu() {
  const { me, logout } = useAuth()
  const signOut = useMutation(authMutations.logout(logout))
  const { theme, setTheme } = useTheme()
  const navigate = useNavigate()
  if (!me) return null

  const name = me.username || me.email || "Signed in"
  const initials = name.slice(0, 2).toUpperCase()
  const dark =
    theme === "dark" ||
    (theme === "system" &&
      window.matchMedia("(prefers-color-scheme: dark)").matches)

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            variant="ghost"
            size="icon"
            aria-label="Account menu"
            className="rounded-lg p-0"
          >
            <Avatar className="size-7 rounded-md">
              <AvatarFallback className="rounded-md text-xs">
                {initials}
              </AvatarFallback>
            </Avatar>
          </Button>
        }
      />
      <DropdownMenuContent side="bottom" align="end" className="min-w-56">
        <DropdownMenuGroup>
          <DropdownMenuLabel className="font-normal">
            <div className="grid leading-tight">
              <span className="text-sm font-medium">{name}</span>
              {me.email && (
                <span className="text-xs text-muted-foreground">
                  {me.email}
                </span>
              )}
            </div>
          </DropdownMenuLabel>
        </DropdownMenuGroup>
        <DropdownMenuSeparator />
        <DropdownMenuItem onClick={() => navigate("/settings")}>
          <HugeiconsIcon icon={Settings01Icon} />
          Settings
        </DropdownMenuItem>
        <DropdownMenuItem onClick={() => setTheme(dark ? "light" : "dark")}>
          {dark ? (
            <HugeiconsIcon icon={Sun01Icon} />
          ) : (
            <HugeiconsIcon icon={MoonIcon} />
          )}
          {dark ? "Light mode" : "Dark mode"}
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem
          disabled={signOut.isPending}
          onClick={() =>
            signOut.mutate(undefined, {
              onSuccess: () => navigate("/login"),
            })
          }
        >
          <HugeiconsIcon icon={Logout01Icon} />
          {signOut.isPending ? "Signing out…" : "Sign out"}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
