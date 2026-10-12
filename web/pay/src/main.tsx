import "./csp"

import { StrictMode } from "react"
import { createRoot } from "react-dom/client"

import { App } from "./app"
import { SIGN_IN_PATH } from "./network"
import { orderIdFromPath, takeSecret } from "./secret"
import { SignInReturn } from "./sign-in"
import "./styles.css"

const orderId = orderIdFromPath()
const root = createRoot(document.getElementById("root")!)
root.render(
  <StrictMode>
    {window.location.pathname === SIGN_IN_PATH ? (
      <SignInReturn />
    ) : orderId ? (
      <App orderId={orderId} secret={takeSecret(orderId)} />
    ) : (
      <p>Not found.</p>
    )}
  </StrictMode>
)
