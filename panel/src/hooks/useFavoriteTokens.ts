import { useCallback, useMemo, useState } from 'react'

// Favorite tokens for the /trade page's token picker (2026-09-19), localStorage-backed per-viewer
// convenience — same discipline as useTradeDefaults: never trusted server-side, wrapped in
// try/catch throughout, degrades to "no favorites" rather than crashing the picker.

const STORAGE_KEY = 'okxbot.favoriteTokens.v1'

function load(): string[] {
  try {
    const raw = localStorage.getItem(STORAGE_KEY)
    if (!raw) return []
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed.filter((s) => typeof s === 'string') : []
  } catch {
    return []
  }
}

function save(symbols: string[]) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(symbols))
  } catch {
    // Private window / blocked storage — the star just won't stick between visits.
  }
}

export function useFavoriteTokens() {
  const [list, setList] = useState<string[]>(() => load())
  const favorites = useMemo(() => new Set(list), [list])

  const toggle = useCallback((symbol: string) => {
    setList((prev) => {
      const next = prev.includes(symbol) ? prev.filter((s) => s !== symbol) : [...prev, symbol]
      save(next)
      return next
    })
  }, [])

  const isFavorite = useCallback((symbol: string) => favorites.has(symbol), [favorites])

  return { favorites, toggle, isFavorite }
}
