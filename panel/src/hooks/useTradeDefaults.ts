import { useCallback, useState } from 'react'

// Per-viewer trade-ticket defaults (margin mode + leverage), persisted in localStorage so they
// survive a token switch or a page refresh instead of resetting to a hardcoded default every time
// (explicit operator requirement, 2026-09-19 Trade page fixes). This is the panel's first use of
// localStorage — deliberately narrow: these are per-viewer conveniences (what the ticket pre-fills
// with), never state the backend trusts or that must be consistent across viewers. Every read/write
// is wrapped in try/catch per this project's own established browser-storage discipline (a private
// window, cleared site data, or a blocked accessor must not crash the page).

const STORAGE_KEY = 'okxbot.tradeDefaults.v1'

export type TdMode = 'cross' | 'isolated'

interface TradeDefaults {
  tdMode: TdMode
  leverage: string
}

const DEFAULTS: TradeDefaults = { tdMode: 'cross', leverage: '10' }

function load(): TradeDefaults {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return DEFAULTS
    const parsed = JSON.parse(raw)
    return {
      tdMode: parsed.tdMode === 'isolated' ? 'isolated' : 'cross',
      leverage: typeof parsed.leverage === 'string' && parsed.leverage.trim() !== '' ? parsed.leverage : DEFAULTS.leverage,
    }
  } catch {
    return DEFAULTS
  }
}

function save(v: TradeDefaults) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(v))
  } catch {
    // Private window / blocked storage / quota — the ticket still works, it just won't remember
    // the choice next time. Never worth surfacing to the operator.
  }
}

export function useTradeDefaults() {
  const [defaults, setDefaults] = useState<TradeDefaults>(() => load())

  const setTdMode = useCallback((tdMode: TdMode) => {
    setDefaults((prev) => {
      const next = { ...prev, tdMode }
      save(next)
      return next
    })
  }, [])

  const setLeverage = useCallback((leverage: string) => {
    setDefaults((prev) => {
      const next = { ...prev, leverage }
      save(next)
      return next
    })
  }, [])

  return { tdMode: defaults.tdMode, leverage: defaults.leverage, setTdMode, setLeverage }
}
