import { createContext, useContext } from "react"

export const AdminUIContext = createContext<{
  namespace: string
  messages?: Record<string, string>
}>({ namespace: "" })

export function useAdminUI() {
  const context = useContext(AdminUIContext)
  function id(value: string): string
  function id(value: string | undefined): string | undefined
  function id(value: string | undefined) {
    return value === undefined ? undefined : `${context.namespace}${value}`
  }
  return {
    text: (message: string) => context.messages?.[message] ?? message,
    id,
  }
}

export function AdminMessage({ id }: { id: string }) {
  return useAdminUI().text(id)
}
