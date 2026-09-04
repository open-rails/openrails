// App shell sidebar — same anatomy as the openrails-saas product shell
// (inset variant, brand header, nav, footer user menu, rail).
import { HugeiconsIcon } from "@hugeicons/react"
import {
  CreditCardIcon,
  DashboardCircleIcon,
  PackageIcon,
  RepeatIcon,
  Settings01Icon,
  Tick02Icon,
  UnfoldMoreIcon,
  UserGroupIcon,
  Wrench01Icon,
} from "@hugeicons/core-free-icons"
import { Link, useLocation } from "react-router-dom"

import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
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
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
  SidebarRail,
} from "@/components/ui/sidebar"
import { useAuth } from "@/lib/auth"

interface NavItem {
  title: string
  url: string
  icon: typeof DashboardCircleIcon
  // Sub-items appear beneath the entry once the section is the one being
  // looked at; they are not a permanently expanded tree.
  items?: { title: string; url: string }[]
}

const nav: NavItem[] = [
  { title: "Dashboard", url: "/", icon: DashboardCircleIcon },
  { title: "Customers", url: "/customers", icon: UserGroupIcon },
  { title: "Subscriptions", url: "/subscriptions", icon: RepeatIcon },
  { title: "Payments", url: "/payments", icon: CreditCardIcon },
  { title: "Invoices", url: "/invoices", icon: CreditCardIcon },
  {
    title: "Catalog",
    url: "/catalog",
    icon: PackageIcon,
    items: [
      { title: "Products", url: "/catalog" },
      { title: "Prices", url: "/catalog/prices" },
      { title: "Metering", url: "/catalog/metering" },
      { title: "Drift", url: "/catalog/drift" },
    ],
  },
  { title: "Ops", url: "/ops", icon: Wrench01Icon },
  { title: "Settings", url: "/settings", icon: Settings01Icon },
]

// The Products sub-page IS /catalog, so it can only be the active one when
// nothing deeper is selected.
function subItemIsActive(pathname: string, url: string, sectionURL: string) {
  return url === sectionURL ? pathname === url : pathname.startsWith(url)
}

export function AppSidebar() {
  const { pathname } = useLocation()
  return (
    <Sidebar variant="inset" collapsible="icon">
      <SidebarHeader>
        {/* No product lockup: this console mounts inside the host's own app
            (doujins, cozy-art), where a vendor mark belongs to someone else's
            product. The merchant switcher is the orientation that matters here,
            and it names the merchant and role rather than the software. */}
        <MerchantSwitcher />
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Billing</SidebarGroupLabel>
          <SidebarGroupContent>
            <SidebarMenu>
              {nav.map((item) => {
                const inSection =
                  item.url === "/"
                    ? pathname === "/"
                    : pathname.startsWith(item.url)
                return (
                  <SidebarMenuItem key={item.url}>
                    <SidebarMenuButton
                      isActive={inSection}
                      tooltip={item.title}
                      render={
                        <Link to={item.url}>
                          <HugeiconsIcon icon={item.icon} />
                          <span>{item.title}</span>
                        </Link>
                      }
                    />
                    {item.items && inSection && (
                      <SidebarMenuSub>
                        {item.items.map((sub) => (
                          <SidebarMenuSubItem key={sub.url}>
                            <SidebarMenuSubButton
                              isActive={subItemIsActive(
                                pathname,
                                sub.url,
                                item.url
                              )}
                              render={
                                <Link to={sub.url}>
                                  <span>{sub.title}</span>
                                </Link>
                              }
                            />
                          </SidebarMenuSubItem>
                        ))}
                      </SidebarMenuSub>
                    )}
                  </SidebarMenuItem>
                )
              })}
            </SidebarMenu>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>
      <SidebarRail />
    </Sidebar>
  )
}

function MerchantSwitcher() {
  const { activeMerchant, merchants, selectMerchant } = useAuth()
  const label = activeMerchant?.instance_slug ?? "Select merchant"
  const role = activeMerchant?.role ?? "Merchant console"
  const initials = activeMerchant?.instance_slug.slice(0, 2) ?? "µ"

  return (
    <SidebarMenu>
      <SidebarMenuItem>
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <SidebarMenuButton
                size="lg"
                tooltip={label}
                className="data-open:bg-sidebar-accent"
              >
                <span className="flex aspect-square size-8 shrink-0 items-center justify-center rounded-lg bg-primary text-xs font-semibold text-primary-foreground uppercase">
                  {initials}
                </span>
                <span className="grid min-w-0 flex-1 text-left leading-tight">
                  <span className="truncate text-sm font-medium">{label}</span>
                  <span className="truncate text-xs text-muted-foreground capitalize">
                    {role}
                  </span>
                </span>
                <HugeiconsIcon
                  icon={UnfoldMoreIcon}
                  className="ml-auto size-4 shrink-0"
                />
              </SidebarMenuButton>
            }
          />
          <DropdownMenuContent side="bottom" align="start" className="min-w-60">
            <DropdownMenuGroup>
              <DropdownMenuLabel className="text-xs text-muted-foreground">
                Merchants
              </DropdownMenuLabel>
              {merchants.map((merchant) => {
                const active =
                  merchant.instance_slug === activeMerchant?.instance_slug
                return (
                  <DropdownMenuItem
                    key={merchant.instance_slug}
                    onClick={() => selectMerchant(merchant.instance_slug)}
                    className="gap-2 py-2"
                  >
                    <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-muted text-[10px] font-semibold uppercase">
                      {merchant.instance_slug.slice(0, 2)}
                    </span>
                    <span className="grid min-w-0 flex-1 leading-tight">
                      <span className="truncate text-sm">
                        {merchant.instance_slug}
                      </span>
                      <span className="truncate text-xs text-muted-foreground capitalize">
                        {merchant.role}
                      </span>
                    </span>
                    {active && (
                      <HugeiconsIcon
                        icon={Tick02Icon}
                        className="ml-auto size-4 shrink-0 text-primary"
                      />
                    )}
                  </DropdownMenuItem>
                )
              })}
              {/* No "create merchant" entry: this console administers one
                  merchant and has no endpoint that makes another. It used to
                  link to /account, a route only the hosted product serves, so
                  on a standalone deployment the menu item led to a 404. */}
            </DropdownMenuGroup>
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  )
}
