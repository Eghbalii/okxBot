import { Component, type ErrorInfo, type ReactNode } from 'react'

// Top-level safety net (added after a real incident: an unnormalized `null` array field from one
// API response threw inside a render, and with no boundary anywhere React unmounted the entire
// page to blank — the panel flashed its nav for a moment then went white, with no visible error
// for the operator to act on). Catches any render error below it and shows what broke instead of
// silently vanishing. Does not catch errors in event handlers/async code (React's error boundaries
// never do) — those still need their own try/catch, same as before.
export default class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state: { error: Error | null } = { error: null }

  static getDerivedStateFromError(error: Error) {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('Panel crashed:', error, info.componentStack)
  }

  render() {
    if (this.state.error) {
      return (
        <div style={{ padding: '2rem', maxWidth: '640px', margin: '0 auto' }}>
          <div className="error-banner">
            Something went wrong rendering this page: {this.state.error.message}
          </div>
          <button onClick={() => this.setState({ error: null })} style={{ marginTop: '0.75rem' }}>
            Try again
          </button>
        </div>
      )
    }
    return this.props.children
  }
}
