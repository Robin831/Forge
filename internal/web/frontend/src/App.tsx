import { Navigate, Route, Routes } from 'react-router-dom'
import { useAuth } from './auth'
import LoginPage from './pages/LoginPage'
import DashboardPage from './pages/DashboardPage'
import IngotsPage from './pages/IngotsPage'
import CruciblesPage from './pages/CruciblesPage'
import HistoryPage from './pages/HistoryPage'
import PRsPage from './pages/PRsPage'
import CostsPage from './pages/CostsPage'
import BeadDetailPage from './pages/BeadDetailPage'

function ProtectedRoute({ children }: { children: React.ReactNode }) {
  const { authenticated, loading } = useAuth()
  if (loading) {
    return (
      <div className="flex h-full items-center justify-center">
        <div
          className="h-8 w-8 animate-spin rounded-full border-2 border-slate-700 border-t-amber-400"
          role="status"
          aria-live="polite"
          aria-label="Loading"
        />
      </div>
    )
  }
  if (!authenticated) {
    return <Navigate to="/login" replace />
  }
  return <>{children}</>
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<LoginPage />} />
      <Route
        path="/"
        element={
          <ProtectedRoute>
            <DashboardPage />
          </ProtectedRoute>
        }
      />
      <Route
        path="/ingots"
        element={
          <ProtectedRoute>
            <IngotsPage />
          </ProtectedRoute>
        }
      />
      <Route
        path="/crucibles"
        element={
          <ProtectedRoute>
            <CruciblesPage />
          </ProtectedRoute>
        }
      />
      <Route
        path="/history"
        element={
          <ProtectedRoute>
            <HistoryPage />
          </ProtectedRoute>
        }
      />
      <Route
        path="/prs"
        element={
          <ProtectedRoute>
            <PRsPage />
          </ProtectedRoute>
        }
      />
      <Route
        path="/costs"
        element={
          <ProtectedRoute>
            <CostsPage />
          </ProtectedRoute>
        }
      />
      <Route
        path="/bead/:bead_id"
        element={
          <ProtectedRoute>
            <BeadDetailPage />
          </ProtectedRoute>
        }
      />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
