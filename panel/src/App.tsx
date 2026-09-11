import { Navigate, NavLink, Route, Routes, useLocation } from 'react-router-dom'
import NotificationBell from './components/NotificationBell'
import './App.css'
import ResourcesPage from './pages/ResourcesPage'
import ModelStatusPage from './pages/ModelStatusPage'
import StrategiesPage from './pages/StrategiesPage'
import PositionsPage from './pages/PositionsPage'
import StrategyTesterPage from './pages/StrategyTesterPage'

const tabs = [
  { to: '/positions/paper', label: 'Positions' },
  { to: '/strategies', label: 'Strategies' },
  { to: '/strategy-tester', label: 'Strategy Tester' },
  { to: '/model', label: 'RL Model' },
  { to: '/resources', label: 'Resources' },
]

export default function App() {
  const location = useLocation()
  return (
    <div className="app">
      <header className="app-header">
        <span className="app-title">okxBot Panel</span>
        <nav className="app-nav">
          {tabs.map((tab) => {
            // Positions' own link target is /positions/paper, but /positions/real should still
            // highlight this tab — match on the /positions prefix rather than the exact path.
            const active =
              tab.to === '/positions/paper'
                ? location.pathname.startsWith('/positions')
                : location.pathname === tab.to
            return (
              <NavLink key={tab.to} to={tab.to} className={'nav-link' + (active ? ' active' : '')}>
                {tab.label}
              </NavLink>
            )
          })}
        </nav>
        {/* In the header rather than on the positions page: an exchange failure matters wherever
            the operator happens to be, and the count has to read the same on every tab. */}
        <NotificationBell />
      </header>
      <main className="app-main">
        <Routes>
          <Route path="/" element={<Navigate to="/positions/paper" replace />} />
          <Route path="/positions" element={<Navigate to="/positions/paper" replace />} />
          <Route path="/positions/:mode" element={<PositionsPage />} />
          <Route path="/strategies" element={<StrategiesPage />} />
          <Route path="/strategy-tester" element={<StrategyTesterPage />} />
          <Route path="/model" element={<ModelStatusPage />} />
          <Route path="/resources" element={<ResourcesPage />} />
        </Routes>
      </main>
    </div>
  )
}
