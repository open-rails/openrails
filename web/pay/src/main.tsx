import "./csp"

import { StrictMode } from "react"
import { createRoot } from "react-dom/client"

import { App } from "./app"
import { orderIdFromPath, takeFragment } from "./secret"
import "./styles.css"

const orderId = orderIdFromPath()
const fragment = orderId ? takeFragment(orderId) : { secret: null }
const root = createRoot(document.getElementById("root")!)
root.render(
  <StrictMode>
    {orderId ? (
      <App orderId={orderId} secret={fragment.secret} fallbacks={fragment.fallbacks} />
    ) : (
      <p>Not found.</p>
    )}
  </StrictMode>
)
