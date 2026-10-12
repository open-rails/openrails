import type { ComponentProps, ReactNode } from "react"
import {
  MemoryRouter as HostRouter,
  useLocation,
  useNavigate,
} from "react-router-dom"
import { AdminRouter, RouteParams } from "../router"
import { matchAdminRoute } from "../routes"

function Bridge({ children }: { children: ReactNode }) {
  const location = useLocation(),
    navigate = useNavigate()
  return (
    <AdminRouter
      navigation={{
        pathname: location.pathname,
        search: location.search,
        navigate,
      }}
    >
      <RouteParams params={matchAdminRoute(location.pathname)?.params ?? {}}>
        {children}
      </RouteParams>
    </AdminRouter>
  )
}
export function MemoryRouter({
  children,
  ...props
}: ComponentProps<typeof HostRouter>) {
  return (
    <HostRouter {...props}>
      <Bridge>{children}</Bridge>
    </HostRouter>
  )
}
