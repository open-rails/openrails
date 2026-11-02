import { HugeiconsIcon } from "@hugeicons/react"
import { Cancel01Icon, Search01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { useMutation } from "@tanstack/react-query"

import { Button } from "#orck/admin/components/ui/button"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import { useAdminServices } from "#orck/admin/context"
import { CUSTOMER_MATCHES } from "#orck/admin/lib/mutations"
import { customerName, shortId } from "#orck/admin/lib/format"
import type { Customer } from "#orck/client/generated/wire"
import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"

// CustomerSearch narrows a list to one customer. The lists have no free-text
// search, so the term is resolved to customers first: one match is taken, and
// several are offered to choose from rather than silently taking the first.
export function CustomerSearch({
  onPick,
}: {
  onPick: (customer: Customer) => void
}) {
  const { toast, toastApiError, adminMutations } = useAdminServices()
  const { text, id } = useAdminUI()
  const lookup = useMutation(adminMutations.findCustomers())
  const [input, setInput] = React.useState("")
  const [matches, setMatches] = React.useState<Customer[] | null>(null)
  const form = React.useRef<HTMLFormElement>(null)
  const first = React.useRef<HTMLButtonElement>(null)
  const field = React.useRef<HTMLInputElement>(null)

  React.useEffect(() => {
    if (!matches) return
    first.current?.focus()
    const outside = (event: PointerEvent) => {
      if (!form.current?.contains(event.target as Node)) setMatches(null)
    }
    document.addEventListener("pointerdown", outside)
    return () => document.removeEventListener("pointerdown", outside)
  }, [matches])

  const pick = (customer: Customer) => {
    setMatches(null)
    setInput("")
    onPick(customer)
  }

  const search = async () => {
    const term = input.trim()
    if (!term) return
    try {
      const found = await lookup.mutateAsync(term)
      if (!found.length) toast.info(text("No customer matches"))
      else if (found.length === 1) pick(found[0])
      else setMatches(found)
    } catch (err) {
      toastApiError(err, "Search customers")
    }
  }

  const shown = matches?.slice(0, CUSTOMER_MATCHES)
  return (
    <form
      ref={form}
      role="search"
      className="relative flex w-full items-center gap-2 sm:w-auto"
      aria-busy={lookup.isPending}
      onSubmit={(event) => {
        event.preventDefault()
        void search()
      }}
      onKeyDown={(event) => {
        if (event.key === "Escape" && matches) {
          event.stopPropagation()
          setMatches(null)
          field.current?.focus()
        }
      }}
    >
      <Label htmlFor={id("customer-search")} className="sr-only">
        <AdminMessage id={"Search customers"} />
      </Label>
      <div className="relative min-w-0 flex-1 sm:flex-none">
        <HugeiconsIcon
          icon={Search01Icon}
          aria-hidden
          className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground"
        />
        <Input
          ref={field}
          id={id("customer-search")}
          className="w-full pr-8 pl-8 sm:w-64"
          placeholder={text("Customer email, ref, or id")}
          value={input}
          disabled={lookup.isPending}
          onChange={(event) => {
            setInput(event.target.value)
            setMatches(null)
          }}
        />
        {input && (
          <Button
            type="button"
            variant="ghost"
            size="icon-xs"
            className="absolute top-1/2 right-1 -translate-y-1/2"
            aria-label={text("Clear search")}
            onClick={() => {
              setInput("")
              setMatches(null)
            }}
          >
            <HugeiconsIcon icon={Cancel01Icon} />
          </Button>
        )}
      </div>
      <Button
        type="submit"
        variant="outline"
        disabled={!input.trim() || lookup.isPending}
      >
        {text(lookup.isPending ? "Searching…" : "Search")}
      </Button>
      {shown && (
        <div className="absolute top-full right-0 left-0 z-20 mt-1 rounded-lg border bg-popover p-1 text-popover-foreground shadow-md sm:left-auto sm:w-80">
          <p
            className="px-2 py-1.5 text-xs text-muted-foreground"
            role="status"
          >
            {matches!.length > CUSTOMER_MATCHES
              ? text(
                  "More customers match. Choose one, or search for more of the name or email."
                )
              : `${matches!.length} ${text("customers match. Choose one.")}`}
          </p>
          <ul className="grid gap-0.5">
            {shown.map((customer, index) => (
              <li key={customer.id}>
                <button
                  ref={index === 0 ? first : undefined}
                  type="button"
                  className="flex min-h-10 w-full flex-col items-start rounded-md px-2 py-1.5 text-left text-sm outline-none hover:bg-muted focus-visible:bg-muted focus-visible:ring-2 focus-visible:ring-ring/50"
                  onClick={() => pick(customer)}
                >
                  <span className="font-medium">{customerName(customer)}</span>
                  <span className="text-xs text-muted-foreground">
                    {[
                      customer.contact?.email !== customerName(customer) &&
                        customer.contact?.email,
                      shortId(customer.id, 13),
                    ]
                      .filter(Boolean)
                      .join(" · ")}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        </div>
      )}
    </form>
  )
}
