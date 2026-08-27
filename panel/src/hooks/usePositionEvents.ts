import { useEffect, useRef } from 'react'
import { openEventsSocket } from '../api/client'

// usePositionEvents connects to cmd/api's WebSocket bridge (CLAUDE.md §11.4/§12) and calls
// onEvent whenever a paper order opens or closes — real-time push instead of waiting for the next
// 5s poll tick. Callers typically use this to trigger an immediate refetch (see PositionsPage),
// letting usePositionAlerts' existing snapshot-diff logic fire the actual sound/notification off
// of that fresher data, rather than reimplementing alerting here.
export function usePositionEvents(onEvent: () => void, enabled: boolean) {
  const onEventRef = useRef(onEvent)
  onEventRef.current = onEvent

  useEffect(() => {
    if (!enabled) return
    return openEventsSocket(() => onEventRef.current())
  }, [enabled])
}
