// App shell sidebar — same anatomy as the hosted product's shell
// (inset variant, brand header, nav, footer user menu, rail).
import { HugeiconsIcon } from "@hugeicons/react"
import {
  Add01Icon,
  Tick02Icon,
  UnfoldMoreIcon,
} from "@hugeicons/core-free-icons"
import { Link, useLocation, useNavigate } from "react-router-dom"

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
import {
  activeNavPath,
  buildNav,
  newMerchantPath,
  useExtensions,
} from "@/extensions/registry"
import type { ConsoleNavItem } from "@/extensions/types"
import { useConsoleContextFor } from "@/extensions/use-console"
import { useAuth } from "@/lib/auth"

// A section's first sub-page IS the section URL (/catalog, /payments), so it
// can only be the active one when nothing deeper is selected.
function subItemIsActive(pathname: string, url: string, sectionURL: string) {
  return url === sectionURL ? pathname === url : pathname.startsWith(url)
}

export function AppSidebar() {
  const { pathname } = useLocation()
  const { extensions } = useExtensions()
  const groups = buildNav(extensions, useConsoleContextFor())
  const active = activeNavPath(
    pathname,
    groups.flatMap((group) => group.items)
  )
  return (
    <Sidebar variant="inset" collapsible="icon">
      <SidebarHeader>
        {/* No product lockup: this console mounts inside the host's own app
            (host-one, host-three), where a vendor mark belongs to someone else's
            product. The merchant switcher is the orientation that matters here,
            and it names the merchant and role rather than the software. */}
        <MerchantSwitcher />
      </SidebarHeader>
      <SidebarContent>
        {groups.map((group) => (
          <SidebarGroup key={group.label}>
            <SidebarGroupLabel>{group.label}</SidebarGroupLabel>
            <SidebarGroupContent>
              <SidebarMenu>
                {group.items.map((item) => (
                  <NavEntry
                    key={item.path}
                    item={item}
                    active={item.path === active}
                    pathname={pathname}
                  />
                ))}
              </SidebarMenu>
            </SidebarGroupContent>
          </SidebarGroup>
        ))}
      </SidebarContent>
      <SidebarRail />
    </Sidebar>
  )
}

const always = () => true

function NavEntry({
  item,
  active,
  pathname,
}: {
  item: ConsoleNavItem
  active: boolean
  pathname: string
}) {
  // A fixed hook per entry: an extension's item list never changes.
  const visible = (item.useVisible ?? always)()
  if (!visible) return null
  return (
    <SidebarMenuItem>
      <SidebarMenuButton
        isActive={active}
        tooltip={item.title}
        render={
          <Link to={item.path}>
            {item.icon && <HugeiconsIcon icon={item.icon} />}
            <span>{item.title}</span>
          </Link>
        }
      />
      {item.items && active && (
        <SidebarMenuSub>
          {item.items.map((sub) => (
            <SidebarMenuSubItem key={sub.path}>
              <SidebarMenuSubButton
                isActive={subItemIsActive(pathname, sub.path, item.path)}
                render={
                  <Link to={sub.path}>
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
}

function MerchantSwitcher() {
  const { activeMerchant, merchants, opensByName, selectMerchant } = useAuth()
  const navigate = useNavigate()
  const newMerchant = newMerchantPath(useExtensions().extensions)
  const label =
    activeMerchant?.display_name || activeMerchant?.slug || "Select merchant"
  const role = activeMerchant?.role || "Merchant console"
  const initials = activeMerchant?.slug.slice(0, 2) ?? "µ"

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
                const active = merchant.slug === activeMerchant?.slug
                return (
                  <DropdownMenuItem
                    key={merchant.slug}
                    onClick={() => selectMerchant(merchant.slug)}
                    className="gap-2 py-2"
                  >
                    <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-muted text-[10px] font-semibold uppercase">
                      {merchant.slug.slice(0, 2)}
                    </span>
                    <span className="grid min-w-0 flex-1 leading-tight">
                      <span className="truncate text-sm">
                        {merchant.display_name || merchant.slug}
                      </span>
                      {merchant.role && (
                        <span className="truncate text-xs text-muted-foreground capitalize">
                          {merchant.role}
                        </span>
                      )}
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
            </DropdownMenuGroup>
            {/* The console creates no merchants itself: the entry exists only
                when a host extension serves a creation page. */}
            {opensByName && (
              <>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  onClick={() => selectMerchant("")}
                  className="gap-2 py-2"
                >
                  Open another merchant
                </DropdownMenuItem>
              </>
            )}
            {newMerchant && (
              <>
                <DropdownMenuSeparator />
                <DropdownMenuItem
                  onClick={() => void navigate(newMerchant)}
                  className="gap-2 py-2"
                >
                  <span className="flex size-7 shrink-0 items-center justify-center rounded-md border border-dashed border-border">
                    <HugeiconsIcon icon={Add01Icon} className="size-4" />
                  </span>
                  New merchant
                </DropdownMenuItem>
              </>
            )}
          </DropdownMenuContent>
        </DropdownMenu>
      </SidebarMenuItem>
    </SidebarMenu>
  )
}
