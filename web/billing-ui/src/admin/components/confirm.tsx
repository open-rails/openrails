// Promise-based confirmation for the console's plain (non-typed) confirms:
//
//   const confirm = useConfirm()
//   if (await confirm({ title, description, confirmLabel, tone })) …
//
// One dialog per ConfirmProvider; a newer request answers an older one false.
// Destructive acts that need a typed word use TypedConfirmDialog instead.
import * as React from "react"

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "#orck/admin/components/ui/alert-dialog"
import {
  ConfirmContext,
  type Confirm,
  type ConfirmOptions,
} from "#orck/admin/lib/confirm"

export function ConfirmProvider({ children }: { children: React.ReactNode }) {
  const [options, setOptions] = React.useState<ConfirmOptions | null>(null)
  const [open, setOpen] = React.useState(false)
  const pending = React.useRef<((answer: boolean) => void) | null>(null)

  const settle = React.useCallback((answer: boolean) => {
    pending.current?.(answer)
    pending.current = null
    setOpen(false)
  }, [])

  const confirm = React.useCallback<Confirm>(
    (next) =>
      new Promise<boolean>((resolve) => {
        pending.current?.(false)
        pending.current = resolve
        setOptions(next)
        setOpen(true)
      }),
    []
  )

  React.useEffect(() => () => pending.current?.(false), [])

  return (
    <ConfirmContext.Provider value={confirm}>
      {children}
      <AlertDialog
        open={open}
        onOpenChange={(next) => {
          if (!next) settle(false)
        }}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{options?.title}</AlertDialogTitle>
            {options?.description ? (
              <AlertDialogDescription>
                {options.description}
              </AlertDialogDescription>
            ) : null}
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>
              {options?.cancelLabel ?? "Cancel"}
            </AlertDialogCancel>
            <AlertDialogAction
              variant={
                options?.tone === "destructive" ? "destructive" : "default"
              }
              onClick={() => settle(true)}
            >
              {options?.confirmLabel ?? "Confirm"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </ConfirmContext.Provider>
  )
}
