import { cn } from "cn"
import { HugeiconsIcon } from "@hugeicons/react"
import { Loading03Icon } from "@hugeicons/core-free-icons"

import { useMessages } from "#orck/i18n/context"

// Local: HugeiconsIcon props, since its strokeWidth rejects the svg string type.
function Spinner({
  className,
  ...props
}: Omit<React.ComponentProps<typeof HugeiconsIcon>, "icon">) {
  const { t } = useMessages() // Local: i18n "Loading" label.
  return (
    <HugeiconsIcon
      icon={Loading03Icon}
      strokeWidth={2}
      data-slot="spinner"
      role="status"
      aria-label={t("common.loading")}
      className={cn("size-4 animate-spin", className)}
      {...props}
    />
  )
}

export { Spinner }
