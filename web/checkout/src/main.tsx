import "./csp"

import { StrictMode } from "react"
import { createRoot } from "react-dom/client"

import { App } from "./app"
import { orderIdFromPath, takeSecret } from "./secret"
import "./styles.css"

const orderId = orderIdFromPath()
const root = createRoot(document.getElementById("root")!)
root.render(
  <StrictMode>{orderId ? <App orderId={orderId} secret={takeSecret(orderId)} /> : <p>Not found.</p>}</StrictMode>
)
