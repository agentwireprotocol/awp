import { StrictMode } from "react"
import { createRoot } from "react-dom/client"

import "./index.css"
import App from "./App.tsx"
import { ThemeProvider } from "@/components/theme-provider.tsx"
import { http, type Transport } from "@/lib/api"
import { installUiSounds } from "@/lib/sound"
import { AWPStore, setStore } from "@/lib/store"

async function transport(): Promise<Transport> {
  const mock = new URLSearchParams(location.search).has("mock") || import.meta.env.VITE_MOCK === "1"
  if (mock) return (await import("@/lib/mock")).mock
  return http
}

transport().then((t) => {
  const store = new AWPStore(t)
  setStore(store)
  store.start()
  installUiSounds()
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <ThemeProvider>
        <App />
      </ThemeProvider>
    </StrictMode>,
  )
})
