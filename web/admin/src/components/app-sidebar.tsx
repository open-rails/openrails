// App shell sidebar — structure adapted from shadcn dashboard-01 /
// satnaing/shadcn-admin.
import {
  CreditCardIcon,
  LayoutDashboardIcon,
  PackageIcon,
  RepeatIcon,
  SettingsIcon,
  UsersIcon,
  WrenchIcon,
  ZapIcon,
} from "lucide-react"
import { Link, useLocation } from "react-router-dom"

import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
} from "@/components/ui/sidebar"

const nav = [
  { title: "Dashboard", url: "/", icon: LayoutDashboardIcon },
  { title: "Customers", url: "/customers", icon: UsersIcon },
  { title: "Subscriptions", url: "/subscriptions", icon: RepeatIcon },
  { title: "Payments", url: "/payments", icon: CreditCardIcon },
  { title: "Catalog", url: "/catalog", icon: PackageIcon },
  { title: "Ops", url: "/ops", icon: WrenchIcon },
  { title: "Settings", url: "/settings", icon: SettingsIcon },
]

export function AppSidebar() {
  const { pathname } = useLocation()
  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton size="lg" asChild>
              <Link to="/">
                <div className="flex aspect-square size-8 items-center justify-center rounded-lg bg-primary text-primary-foreground">
                  <ZapIcon className="size-4" />
                </div>
                <div className="grid flex-1 text-left text-sm leading-tight">
                  <span className="truncate font-medium">OpenRails</span>
                  <span className="truncate text-xs text-muted-foreground">Merchant console</span>
                </div>
              </Link>
            </SidebarMenuButton>
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
                    asChild
                    isActive={
                      item.url === "/" ? pathname === "/" : pathname.startsWith(item.url)
                    }
                    tooltip={item.title}
                  >
                    <Link to={item.url}>
                      <item.icon />
                      <span>{item.title}</span>
                    </Link>
                  </SidebarMenuButton>
                </SidebarMenuItem>
              ))}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>
    </Sidebar>
  )
}
