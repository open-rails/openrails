import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { BrowserRouter, Route, Routes } from "react-router-dom"
import { AuthUiProvider } from "@openrails/auth-ui"
import { AuthProvider } from "@openrails/auth-ui/react"

import { auth, CourseBuyPage, CoursePage, JoinPage, MembersQAPage, StorePage } from "./pages"

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <AuthProvider client={auth}>
      <AuthUiProvider>
        <BrowserRouter>
          <Routes>
            <Route path="/" element={<StorePage />} />
            <Route path="/courses/:course" element={<CoursePage />} />
            <Route path="/courses/:course/buy" element={<CourseBuyPage />} />
            <Route path="/members/qa" element={<MembersQAPage />} />
            <Route path="/join" element={<JoinPage />} />
          </Routes>
        </BrowserRouter>
      </AuthUiProvider>
    </AuthProvider>
  </StrictMode>
)

