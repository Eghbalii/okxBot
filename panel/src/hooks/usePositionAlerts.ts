import { useEffect, useRef } from 'react'
import type { Position } from '../api/types'
import openSound from '../sounds/open.wav'
import tpSound from '../sounds/tp.wav'
import slSound from '../sounds/sl.wav'

// CLAUDE.md §11.4: distinct sound + browser notification for (a) position opened, (b) closed by
// SL, (c) closed by TP. Purely client-side — diffs the previous poll's position list against the
// current one and fires on the transitions it finds; no backend "notification service" needed.
export function usePositionAlerts(positions: Position[] | null, enabled: boolean) {
  const seenRef = useRef<Map<number, Position>>(new Map())
  const firstRunRef = useRef(true)

  useEffect(() => {
    if (enabled && 'Notification' in window && Notification.permission === 'default') {
      Notification.requestPermission()
    }
  }, [enabled])

  useEffect(() => {
    if (!positions) return
    const prevSeen = seenRef.current
    const nextSeen = new Map<number, Position>()

    // On the very first successful load, just record baseline state — don't fire alerts for
    // positions that were already open/closed before the panel connected.
    const isFirstRun = firstRunRef.current
    firstRunRef.current = false

    for (const pos of positions) {
      nextSeen.set(pos.ID, pos)
      if (isFirstRun || !enabled) continue

      const prev = prevSeen.get(pos.ID)
      if (!prev) {
        fireAlert('open', pos)
      } else if (!prev.ClosedAt && pos.ClosedAt) {
        if (pos.CloseReason === 'tp') fireAlert('tp', pos)
        else if (pos.CloseReason === 'sl') fireAlert('sl', pos)
      }
    }

    seenRef.current = nextSeen
  }, [positions, enabled])
}

function fireAlert(kind: 'open' | 'tp' | 'sl', pos: Position) {
  const sound = kind === 'open' ? openSound : kind === 'tp' ? tpSound : slSound
  new Audio(sound).play().catch(() => {
    /* autoplay can be blocked before first user interaction — ignore */
  })

  const titles = {
    open: `Opened ${pos.Side.toUpperCase()} ${pos.InstID}`,
    tp: `TP hit: ${pos.InstID}`,
    sl: `SL hit: ${pos.InstID}`,
  }
  const body =
    kind === 'open'
      ? `entry ${pos.EntryPx} · ${pos.Mode}`
      : `close ${pos.ClosePx} · pnl ${pos.RealizedPnL} · ${pos.Mode}`

  if ('Notification' in window && Notification.permission === 'granted') {
    new Notification(titles[kind], { body })
  }
}
