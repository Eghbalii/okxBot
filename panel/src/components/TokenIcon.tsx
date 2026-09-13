import { useState } from 'react'

// TokenIcon renders a token's logo, falling back to a deterministic letter avatar.
//
// The image comes from a public CDN keyed by lowercase symbol. The fallback is not decoration: this
// bot's roster is populated by a discovery scan that will surface tokens no icon set has heard of, so
// a missing image is the NORMAL case rather than an error, and a broken-image placeholder on a third
// of the table would make the page unreadable.
//
// The panel is VPN-only, not airgapped (CLAUDE.md §11), so an outbound image request is acceptable
// here — but note it is the one in this app, and it fails closed: onError swaps to the avatar, so a
// blocked CDN degrades to the letter form rather than leaving holes.

// Color is derived from the symbol's own characters rather than a random or sequential palette, so a
// token keeps the same color across reloads, pages and sort orders. Recognizing a row by its color is
// only useful if that color is stable.
function colorFor(symbol: string): string {
  let hash = 0
  for (let i = 0; i < symbol.length; i++) {
    hash = (hash * 31 + symbol.charCodeAt(i)) % 360
  }
  // Fixed saturation/lightness keeps every avatar at similar contrast against both themes, which a
  // free-running hue alone would not.
  return `hsl(${hash} 55% 42%)`
}

// Two characters, because one is ambiguous across a roster with several tokens per letter (BTC/BNB,
// SOL/SUI) and three stops fitting the circle at this size.
function initialsFor(symbol: string): string {
  return symbol.replace(/^1000/, '').slice(0, 2).toUpperCase()
}

export default function TokenIcon({ symbol, size = 24 }: { symbol: string; size?: number }) {
  const [failed, setFailed] = useState(false)

  // Leading "1000" is a contract-size prefix (1000PEPE, 1000SHIB), not part of the token's identity,
  // so it is stripped for the icon lookup — otherwise every such token falls back unnecessarily.
  const slug = symbol.replace(/^1000/, '').toLowerCase()
  const style = { width: size, height: size, minWidth: size }

  if (failed) {
    return (
      <span
        className="token-icon token-icon-fallback"
        style={{ ...style, background: colorFor(symbol), fontSize: size * 0.4 }}
        title={symbol}
        aria-label={symbol}
      >
        {initialsFor(symbol)}
      </span>
    )
  }

  return (
    <img
      className="token-icon"
      style={style}
      src={`https://cdn.jsdelivr.net/npm/cryptocurrency-icons@0.18.1/32/color/${slug}.png`}
      alt={symbol}
      title={symbol}
      loading="lazy"
      onError={() => setFailed(true)}
    />
  )
}

// Exchange logo sources, verified against the live CDN (2026-09-13): 294 is OKX (the black
// checkerboard), 544 is MEXC (the blue M). Checked by actually downloading and looking at both —
// an id map is exactly the kind of thing that is silently wrong otherwise.
//
// Unlike token icons, this is a SMALL FIXED SET: an exchange only appears here once an adapter has
// been written for it, so a missing id is a wiring gap to fix rather than the routine case. The
// letter fallback still exists for when the CDN itself is unreachable.
const EXCHANGE_LOGOS: Record<string, string> = {
  okx: 'https://s2.coinmarketcap.com/static/img/exchanges/64x64/294.png',
  mexc: 'https://s2.coinmarketcap.com/static/img/exchanges/64x64/544.png',
}

/**
 * One exchange's round logo, falling back to a colored letter mark.
 *
 * Round rather than square because these are identity marks in a dense table, and a circle reads as
 * "who" where a square reads as "what" — the token icons beside them are already round for the same
 * reason.
 */
export function ExchangeIcon({ exchange, size = 20 }: { exchange: string; size?: number }) {
  const [failed, setFailed] = useState(false)
  const src = EXCHANGE_LOGOS[exchange]
  const style = { width: size, height: size, minWidth: size }

  if (!src || failed) {
    return (
      <span
        className={`exchange-icon exchange-icon-fallback exchange-icon-${exchange}`}
        style={{ ...style, fontSize: size * 0.42 }}
        title={exchange}
        aria-label={exchange}
      >
        {exchange.slice(0, 2).toUpperCase()}
      </span>
    )
  }
  return (
    <img
      className="exchange-icon"
      style={style}
      src={src}
      alt={exchange}
      title={exchange}
      loading="lazy"
      onError={() => setFailed(true)}
    />
  )
}

/**
 * The exchanges a token trades on: round logos, with their names revealed on hover.
 *
 * The name is shown on hover rather than always, because at ten-plus rows the names cost more width
 * than the whole price column and say the same thing twice — but a logo alone is unreadable until
 * you have learned it, so the hover has to be real rather than only a native title tooltip (which is
 * slow to appear and cannot be styled).
 */
export function ExchangeBadges({ exchanges }: { exchanges: string[] }) {
  const sorted = [...exchanges].sort()
  return (
    <span className="exchange-badges" title={sorted.join(', ')}>
      {sorted.map((ex) => (
        <ExchangeIcon key={ex} exchange={ex} />
      ))}
      <span className="exchange-badges-popover" role="tooltip">
        {sorted.map((ex) => (
          <span key={ex} className="exchange-badges-popover-row">
            <ExchangeIcon exchange={ex} size={14} />
            {ex.toUpperCase()}
          </span>
        ))}
      </span>
    </span>
  )
}
