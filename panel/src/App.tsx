import { Navigate, NavLink, Route, Routes, useLocation } from 'react-router-dom'
import NotificationBell from './components/NotificationBell'
import ModeSelect from './components/ModeSelect'
import type { PositionMode } from './api/types'
import './App.css'
import ResourcesPage from './pages/ResourcesPage'
import ModelStatusPage from './pages/ModelStatusPage'
import StrategiesPage from './pages/StrategiesPage'
import PositionsPage from './pages/PositionsPage'
import HomePage from './pages/HomePage'
import StrategyTesterPage from './pages/StrategyTesterPage'
import TradePage from './pages/TradePage'
import AccountPage from './pages/AccountPage'

const tabs = [
  { to: '/home', label: 'Home' },
  { to: '/trade', label: 'Trade' },
  { to: '/positions/paper', label: 'Positions' },
  { to: '/account', label: 'Account' },
  { to: '/strategies', label: 'Strategies' },
  { to: '/strategy-tester', label: 'Strategy Tester' },
  { to: '/model', label: 'RL Model' },
  { to: '/resources', label: 'Resources' },
]

export default function App() {
  const location = useLocation()
  const onPositions = location.pathname.startsWith('/positions')
  // Derived from the URL rather than held in state, so the header and the page can never disagree
  // about which mode is showing. 'manual' was a third value here until 2026-09-20 — manual
  // positions no longer have their own /positions/:mode route at all (see PositionsPage.tsx).
  const mode: PositionMode = location.pathname.startsWith('/positions/bot') ? 'bot' : 'paper'
  return (
    <div className="app">
      <header className="app-header">
        <span className="app-title">okxBot Panel</span>
        <nav className="app-nav">
          {tabs.map((tab) => {
            // Positions' own link target is /positions/paper, but /positions/bot should still
            // highlight this tab — match on the /positions prefix rather than the exact path.
            // Trade is the same story once a token is in the URL (/trade/BTC).
            const active =
              tab.to === '/positions/paper'
                ? location.pathname.startsWith('/positions')
                : tab.to === '/trade'
                  ? location.pathname.startsWith('/trade')
                  : location.pathname === tab.to
            return (
              <NavLink key={tab.to} to={tab.to} className={'nav-link' + (active ? ' active' : '')}>
                {tab.label}
              </NavLink>
            )
          })}
        </nav>
        {/* Right-aligned group. The bell used to sit immediately after the nav, which left it
            floating mid-header; pushing the group to the edge gives it a fixed, findable home. */}
        <div className="app-header-right">
          {/* Only on the positions routes — the only page whose content is mode-scoped. */}
          {onPositions && <ModeSelect mode={mode} />}
          {/* In the header rather than on the positions page: an exchange failure matters wherever
              the operator happens to be, and the count has to read the same on every tab. */}
          <NotificationBell />
        </div>
      </header>
      <main className="app-main">
        <Routes>
          {/* Home is the landing route (2026-09-13 request). Positions keeps its own URLs, so
              existing bookmarks still work. */}
          <Route path="/" element={<Navigate to="/home" replace />} />
          <Route path="/home" element={<HomePage />} />
          <Route path="/positions" element={<Navigate to="/positions/paper" replace />} />
          {/* /positions/manual is a stale bookmark from before 2026-09-20 — manual positions moved
              to the bottom of the Trade page, so this now points there instead of silently
              redirecting to Paper (which PositionsPage's own guard would otherwise do, since
              'manual' is no longer a recognized :mode value there). */}
          <Route path="/positions/manual" element={<Navigate to="/trade" replace />} />
          <Route path="/positions/:mode" element={<PositionsPage />} />
          {/* :symbol is optional, defaulting to BTC inside the page itself (docs/MANUAL_TRADE_PLAN.md
              §5.4) — the default must live in the page, not be silently assumed by a caller that
              renders it with no symbol. */}
          <Route path="/trade" element={<TradePage />} />
          <Route path="/trade/:symbol" element={<TradePage />} />
          <Route path="/account" element={<AccountPage />} />
          <Route path="/strategies" element={<StrategiesPage />} />
          <Route path="/strategy-tester" element={<StrategyTesterPage />} />
          <Route path="/model" element={<ModelStatusPage />} />
          <Route path="/resources" element={<ResourcesPage />} />
        </Routes>
      </main>
    </div>
  )
}
