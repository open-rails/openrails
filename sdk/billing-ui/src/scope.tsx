import type { ComponentProps } from "react"

import type { CheckoutAppearance } from "./appearance"
import { cn } from "cn"
import { useScopeProps } from "./scope-context"

/** Styling root for in-page billing surfaces. */
export function BillingUiRoot({
  appearance,
  className,
  style,
  ...props
}: ComponentProps<"div"> & { appearance?: CheckoutAppearance }) {
  const scope = useScopeProps(appearance)
  return (
    <div
      data-orck-theme={scope["data-orck-theme"]}
      className={cn(scope.className, className)}
      style={{ ...scope.style, ...style }}
      {...props}
    />
  )
}
