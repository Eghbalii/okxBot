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

// ExchangeBadges shows which exchanges carry a token: compact marks by default, with the full list on
// hover, per the Home page's own spec. The marks are letter-based rather than brand logos — an
// exchange's logo is its trademark, and a two-letter mark carries the same information here.
export function ExchangeBadges({ exchanges }: { exchanges: string[] }) {
  const sorted = [...exchanges].sort()
  return (
    <span className="exchange-badges" title={sorted.join(', ')}>
      {sorted.map((ex) => (
        <span key={ex} className={`exchange-badge exchange-badge-${ex}`} aria-label={ex}>
          {ex.slice(0, 2).toUpperCase()}
        </span>
      ))}
      {/* The hover list is a real element rather than only the title attribute: a native tooltip is
          slow to appear and cannot be styled, and this is the page's primary way of answering "where
          can I trade this". */}
      <span className="exchange-badges-popover" role="tooltip">
        {sorted.map((ex) => (
          <span key={ex} className="exchange-badges-popover-row">
            {ex}
          </span>
        ))}
      </span>
    </span>
  )
}
