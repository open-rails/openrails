import "./csp"

import { StrictMode } from "react"
import { createRoot } from "react-dom/client"

import { App } from "./app"
import { invoiceIdFromPath, takeFragment } from "./secret"
import "./styles.css"

const invoiceId = invoiceIdFromPath()
const fragment = invoiceId ? takeFragment(invoiceId) : { secret: null }
const root = createRoot(document.getElementById("root")!)
root.render(
  <StrictMode>
    {invoiceId ? (
      <App invoiceId={invoiceId} secret={fragment.secret} fallbacks={fragment.fallbacks} />
    ) : (
      <p>Not found.</p>
    )}
  </StrictMode>
)
