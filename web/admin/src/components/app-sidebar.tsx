// App shell sidebar — same anatomy as the openrails-saas product shell
// (inset variant, brand header, nav, footer user menu, rail).
import { HugeiconsIcon } from "@hugeicons/react"
import {
  ArrowUpDownIcon,
  CreditCardIcon,
  DashboardCircleIcon,
  Logout01Icon,
  MoonIcon,
  PackageIcon,
  RepeatIcon,
  Settings01Icon,
  Sun01Icon,
  UserGroupIcon,
  Wrench01Icon,
} from "@hugeicons/core-free-icons"
import { Link, useLocation, useNavigate } from "react-router-dom"

import { useTheme } from "@/components/theme-provider"
import { Avatar, AvatarFallback } from "@/components/ui/avatar"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarRail,
} from "@/components/ui/sidebar"
import { useAuth } from "@/lib/auth"

const nav = [
  { title: "Dashboard", url: "/", icon: DashboardCircleIcon },
  { title: "Customers", url: "/customers", icon: UserGroupIcon },
  { title: "Subscriptions", url: "/subscriptions", icon: RepeatIcon },
  { title: "Payments", url: "/payments", icon: CreditCardIcon },
  { title: "Catalog", url: "/catalog", icon: PackageIcon },
  { title: "Ops", url: "/ops", icon: Wrench01Icon },
  { title: "Settings", url: "/settings", icon: Settings01Icon },
]

export function AppSidebar() {
  const { pathname } = useLocation()
  return (
    <Sidebar variant="inset" collapsible="icon">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton
              size="lg"
              render={
                <Link to="/">
                  <div className="flex aspect-square size-8 items-center justify-center rounded-lg bg-primary text-sm font-semibold text-primary-foreground">
                    µ
                  </div>
                  <div className="grid flex-1 text-left text-sm leading-tight">
                    <span className="truncate font-medium">OpenRails</span>
                    <span className="truncate text-xs text-muted-foreground">
                      Merchant console
                    </span>
                  </div>
                </Link>
              }
            />
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Billing</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {nav.map((item) => (
                <SidebarMenuItem key={item.url}>
                  <SidebarMenuButton
                    isActive={
                      item.url === "/"
                        ? pathname === "/"
                        : pathname.startsWith(item.url)
                    }
                    tooltip={item.title}
                    render={
                      <Link to={item.url}>
                        <HugeiconsIcon icon={item.icon} />
                        <span>{item.title}</span>
                      </Link>
                    }
                  />
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>
      <SidebarFooter>
        <UserMenu />
      </SidebarFooter>
      <SidebarRail />
    </Sidebar>
  )
}

function UserMenu() {
  const { me, logout } = useAuth()
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
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <SidebarMenuButton
                size="lg"
                className="data-open:bg-sidebar-accent"
              >
                <Avatar className="size-8 rounded-lg">
                  <AvatarFallback className="rounded-lg">
                    {initials}
                  </AvatarFallback>
                </Avatar>
                <div className="grid flex-1 text-left leading-tight">
                  <span className="truncate text-sm font-medium">{name}</span>
                  {me.email && me.username && (
                    <span className="truncate text-xs text-muted-foreground">
                      {me.email}
                    </span>
                  )}
                </div>
                <HugeiconsIcon
                  icon={ArrowUpDownIcon}
                  className="ml-auto size-4"
                />
              </SidebarMenuButton>
            }
          />
          <DropdownMenuContent side="top" align="start" className="min-w-56">
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
              onClick={async () => {
                await logout()
                navigate("/login")
              }}
            >
              <HugeiconsIcon icon={Logout01Icon} />
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  )
}
