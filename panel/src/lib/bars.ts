// Bar-duration parsing, shared by anything that has to align a timestamp to a candle boundary.
//
// Mirrors go-engine's own internal/usecase/tickfeed.go:barSeconds so a boundary computed here lands
// on the same second the ingestor used — verified against that implementation for every timeframe
// in use. OKX's casing is significant (lowercase 'm' is minutes, capital 'M' is months, §9), and
// getting it wrong would silently bucket a 5m chart into 5-month candles rather than failing
// visibly. Returns 0 for anything unparseable, which callers treat as "don't align".
export function barSeconds(bar: string): number {
  const m = /^(\d+)([mHhDdWw]|M)$/.exec(bar)
  if (!m) return 0
  const n = Number(m[1])
  if (!n) return 0
  switch (m[2]) {
    case 'm':
      return n * 60
    case 'H':
    case 'h':
      return n * 3600
    case 'D':
    case 'd':
      return n * 86400
    case 'W':
    case 'w':
      return n * 604800
    case 'M':
      return n * 2592000
    default:
      return 0
  }
}
