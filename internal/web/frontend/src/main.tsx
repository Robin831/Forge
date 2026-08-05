import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router'
import './index.css'
import App from './App'
import { AuthProvider } from './auth'
import { ToastProvider, ToastViewport } from './hooks/useToast'
import { ResolveStoreProvider } from './stores/resolveStore'

const root = document.getElementById('root')
if (!root) {
  throw new Error('Hearth: #root element not found in index.html')
}

createRoot(root).render(
  <StrictMode>
    <BrowserRouter>
      <AuthProvider>
        <ToastProvider>
          <ResolveStoreProvider>
            <App />
            <ToastViewport />
          </ResolveStoreProvider>
        </ToastProvider>
      </AuthProvider>
    </BrowserRouter>
  </StrictMode>,
)
