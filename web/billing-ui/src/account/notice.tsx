import * as React from "react"
import { cn } from "cn"

/** Polite confirmation line that clears itself. */
export function useNotice(): [React.ReactNode, (text: string) => void] {
  const [text, setText] = React.useState<string | null>(null)
  React.useEffect(() => {
    if (!text) return
    const timer = setTimeout(() => setText(null), 6000)
    return () => clearTimeout(timer)
  }, [text])
  const node = (
    <p
      role="status"
      aria-live="polite"
      className={cn("text-sm text-muted-foreground", text ? "pt-3" : "sr-only")}
    >
      {text}
    </p>
  )
  return [node, setText]
}
