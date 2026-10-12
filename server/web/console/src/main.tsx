import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { BrowserRouter, Route, Routes } from "react-router-dom"
import { Billing } from "./billing"
import "@openrails/billing-ui/admin.css"
import "./style.css"
import { ConsoleSession, createConsoleSession } from "./lib/session"
import { bootstrapURL, routerBasename } from "./lib/mount"
import { LoginPage } from "./pages/login"
import { CallbackPage } from "./pages/callback"
import type { BootstrapConfig } from "./config"

const root = createRoot(document.getElementById("root")!)
fetch(bootstrapURL(), { cache: "no-store" })
  .then(async (response) => {
    if (!response.ok)
      throw new Error(`Console configuration failed (${response.status})`)
    const config = (await response.json()) as BootstrapConfig
    const session = createConsoleSession(config)
    const billing = <Billing config={config} session={session} />
    root.render(
      <StrictMode>
        <ConsoleSession session={session}>
          <BrowserRouter basename={routerBasename()}>
            <Routes>
              <Route path="/login" element={<LoginPage />} />
              {session.kind === "issuer" && (
                <Route path="/callback" element={<CallbackPage />} />
              )}
              <Route path="*" element={billing} />
            </Routes>
          </BrowserRouter>
        </ConsoleSession>
      </StrictMode>
    )
  })
  .catch((error: unknown) =>
    root.render(
      <div role="alert">
        Admin console failed to start:{" "}
        {error instanceof Error ? error.message : String(error)}
      </div>
    )
  )
