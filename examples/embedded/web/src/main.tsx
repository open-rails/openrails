import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { BrowserRouter, Link, Route, Routes } from "react-router-dom"
import { AuthUiProvider } from "@openrails/auth-ui"
import { AuthProvider } from "@openrails/auth-ui/react"

import { auth, BuyCoursePage, CoursePage, JoinPage, MembersQAPage } from "./pages"

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <AuthProvider client={auth}>
      <AuthUiProvider>
        <BrowserRouter>
          <Routes>
            <Route path="/" element={<Home />} />
            <Route path="/courses/:course" element={<CoursePage />} />
            <Route path="/courses/:course/buy" element={<BuyCoursePage />} />
            <Route path="/members/qa" element={<MembersQAPage />} />
            <Route path="/join" element={<JoinPage />} />
          </Routes>
        </BrowserRouter>
      </AuthUiProvider>
    </AuthProvider>
  </StrictMode>
)

function Home() {
  return (
    <ul>
      <li><Link to="/courses/css-101">Intro to CSS</Link></li>
      <li><Link to="/courses/tailwind-102">Intro to Tailwind</Link></li>
      <li><Link to="/members/qa">Members-only Q&A</Link></li>
    </ul>
  )
}
