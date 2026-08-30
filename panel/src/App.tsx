import { NavLink, Route, Routes } from 'react-router-dom'
import './App.css'
import ResourcesPage from './pages/ResourcesPage'
import ModelStatusPage from './pages/ModelStatusPage'
import StrategiesPage from './pages/StrategiesPage'
import PositionsPage from './pages/PositionsPage'
import SLTPComparisonPage from './pages/SLTPComparisonPage'
import StrategyTesterPage from './pages/StrategyTesterPage'

const tabs = [
  { to: '/positions', label: 'Positions' },
  { to: '/strategies', label: 'Strategies' },
  { to: '/strategy-tester', label: 'Strategy Tester' },
  { to: '/sltp-comparison', label: 'SL/TP A-B' },
  { to: '/model', label: 'RL Model' },
  { to: '/resources', label: 'Resources' },
]

export default function App() {
  return (
    <div className="app">
      <header className="app-header">
        <span className="app-title">okxBot Panel</span>
        <nav className="app-nav">
          {tabs.map((tab) => (
            <NavLink
              key={tab.to}
              to={tab.to}
              className={({ isActive }) => 'nav-link' + (isActive ? ' active' : '')}
            >
              {tab.label}
            </NavLink>
          ))}
        </nav>
      </header>
      <main className="app-main">
        <Routes>
          <Route path="/" element={<PositionsPage />} />
          <Route path="/positions" element={<PositionsPage />} />
          <Route path="/strategies" element={<StrategiesPage />} />
          <Route path="/strategy-tester" element={<StrategyTesterPage />} />
          <Route path="/sltp-comparison" element={<SLTPComparisonPage />} />
          <Route path="/model" element={<ModelStatusPage />} />
          <Route path="/resources" element={<ResourcesPage />} />
        </Routes>
      </main>
    </div>
  )
}
