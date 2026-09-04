import { describe, expect, it, vi } from "vitest"
import { renderToStaticMarkup } from "react-dom/server"
import { MemoryRouter } from "react-router-dom"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { InvoiceDetail } from "./detail"
import { InvoiceProfileEditor } from "../customers/invoice-profile"
import type { MerchantInvoice } from "@/lib/api/invoice-types"

vi.mock("@/lib/api/client", () => ({
  getTokens: () => ({ merchant: "a" }),
  api: vi.fn(),
}))
const invoice = (
  actions: MerchantInvoice["available_actions"]
): MerchantInvoice => ({
  id: "invoice-1",
  customer_id: "customer-1",
  currency: "JPY",
  unit_decimals: 4,
  invoice_number: "INV-1",
  status: "open",
  period_from: "2026-09-01T00:00:00Z",
  period_to: "2026-10-01T00:00:00Z",
  total_amount: 120000,
  subtotal_amount: 120000,
  amount_paid: 20000,
  amount_due: 100000,
  collection_method: "send_invoice",
  collection_failure_count: 0,
  line_items: [{ event_type: "usage", amount: 120000, count: 1 }],
  available_actions: actions,
})
function render(node: React.ReactNode) {
  return renderToStaticMarkup(
    <MemoryRouter>
      <QueryClientProvider client={new QueryClient()}>
        {node}
      </QueryClientProvider>
    </MemoryRouter>
  )
}
describe("invoice console rendering", () => {
  it("shows currency-scaled amounts and invoice/customer navigation", () => {
    const html = render(<InvoiceDetail invoice={invoice([])} />)
    expect(html).toContain('href="/invoices"')
    expect(html).toContain('href="/customers/customer-1"')
    expect(html).toContain("12 JPY")
    expect(html).toContain("10 JPY")
    expect(html).not.toContain("0.12 JPY")
    expect(html).not.toContain("Void invoice")
  })
  it("renders only the actions returned for the permission and state", () => {
    const html = render(
      <InvoiceDetail invoice={invoice(["void", "record_payment"])} />
    )
    expect(html).toContain("Void invoice")
    expect(html).toContain("Record payment")
    expect(html).not.toContain("Retry collection")
  })
  it("makes a read-only invoice profile inspectable without a save control", () => {
    const html = render(
      <InvoiceProfileEditor
        customerId="customer-1"
        canUpdate={false}
        profile={{
          net_terms_days: 30,
          collection_method: "send_invoice",
          po_number: "PO-1",
        }}
      />
    )
    expect(html).toContain("PO-1")
    expect(html).toContain("disabled")
    expect(html).not.toContain("Save invoice profile")
  })
})
