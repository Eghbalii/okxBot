package rest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/eghbalii/okxBot/go-engine/internal/domain"
	"github.com/shopspring/decimal"
)

// Every wire shape in this file was verified against the live contract.mexc.com API on 2026-09-13
// rather than transcribed from documentation. Where MEXC's actual response differed from what the
// docs implied, the live response wins and the difference is noted.

// tickerResponse is /api/v1/contract/ticker's data object.
//
// Prices arrive as JSON NUMBERS here, not the strings OKX uses. That matters: float64 cannot
// represent most decimal fractions exactly, which is why this project moved every price to
// decimal.Decimal (§14). json.Number preserves the literal text so the decimal is built from the
// digits MEXC actually sent, rather than from a float that has already lost them.
type tickerResponse struct {
	Symbol     string      `json:"symbol"`
	LastPrice  json.Number `json:"lastPrice"`
	Bid1       json.Number `json:"bid1"`
	Ask1       json.Number `json:"ask1"`
	High24     json.Number `json:"high24Price"`
	Lower24    json.Number `json:"lower24Price"`
	Volume24   json.Number `json:"volume24"`
	Timestamp  int64       `json:"timestamp"`
	FundingRat json.Number `json:"fundingRate"`
}

// GetTicker fetches one instrument's current ticker.
func (c *Client) GetTicker(instID string) (domain.Ticker, error) {
	var resp tickerResponse
	if err := c.doPublic("/api/v1/contract/ticker", map[string]string{"symbol": instID}, &resp); err != nil {
		return domain.Ticker{}, fmt.Errorf("mexc get ticker %s: %w", instID, err)
	}
	return domain.Ticker{
		InstID:  resp.Symbol,
		Last:    num(resp.LastPrice),
		BidPx:   num(resp.Bid1),
		AskPx:   num(resp.Ask1),
		High24h: num(resp.High24),
		Low24h:  num(resp.Lower24),
		Vol24h:  num(resp.Volume24),
		// Open24h has no direct MEXC equivalent (it reports riseFallRate instead). Left zero rather
		// than derived: a computed value here would be indistinguishable from a reported one, and
		// nothing in this codebase reads Open24h today.
	}, nil
}

// klineResponse is /api/v1/contract/kline/{symbol}'s data object.
//
// MEXC returns candles as PARALLEL COLUMN ARRAYS — time[], open[], close[], high[], low[], vol[] —
// where OKX returns an array of row-arrays. This is the single biggest wire-format difference
// between the two adapters, and the reason toCandles below validates that every column has the same
// length before indexing: a short column would otherwise panic at runtime on an index out of range,
// turning a malformed response into a crashed ingestor.
type klineResponse struct {
	Time  []int64       `json:"time"`
	Open  []json.Number `json:"open"`
	Close []json.Number `json:"close"`
	High  []json.Number `json:"high"`
	Low   []json.Number `json:"low"`
	Vol   []json.Number `json:"vol"`
}

// toCandles converts MEXC's column arrays into domain candles, oldest-first.
func (k klineResponse) toCandles() ([]domain.Candle, error) {
	n := len(k.Time)
	// A response whose columns disagree is malformed. Failing here is deliberate: silently
	// truncating to the shortest column would produce candles that pair one bar's timestamp with
	// another's price, which is corrupt data that looks perfectly valid downstream.
	if len(k.Open) != n || len(k.Close) != n || len(k.High) != n || len(k.Low) != n || len(k.Vol) != n {
		return nil, fmt.Errorf("malformed kline response: columns disagree (time=%d open=%d close=%d high=%d low=%d vol=%d)",
			n, len(k.Open), len(k.Close), len(k.High), len(k.Low), len(k.Vol))
	}
	out := make([]domain.Candle, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, domain.Candle{
			// MEXC's kline timestamps are SECONDS (verified live: 1789248000 for a 5m bar), unlike
			// its ticker timestamps which are milliseconds. Mixing these up shifts every candle by
			// ~56000 years, which is at least loud; the reverse (ms read as s) is the quiet one.
			Timestamp: time.Unix(k.Time[i], 0).UTC(),
			Open:      num(k.Open[i]),
			High:      num(k.High[i]),
			Low:       num(k.Low[i]),
			Close:     num(k.Close[i]),
			Volume:    num(k.Vol[i]),
		})
	}
	return out, nil
}

// GetCandles fetches recent candles for one instrument, oldest-first.
//
// bar is this project's internal timeframe ("5m", "1H"); it is translated to MEXC's own interval
// vocabulary here, at the adapter boundary, so no caller needs to know MEXC spells five minutes
// "Min5".
func (c *Client) GetCandles(instID, bar string, limit int) ([]domain.Candle, error) {
	interval, ok := IntervalFor(bar)
	if !ok {
		return nil, fmt.Errorf("mexc: unsupported bar %q", bar)
	}
	params := map[string]string{"interval": interval}
	// MEXC has no "limit" parameter on this endpoint — it takes a start time instead, so the window
	// is derived from the requested count and the bar's own duration. Asking for a start too far
	// back simply returns what exists, which is the desired behaviour for a cold start.
	if limit > 0 {
		if d, ok := BarDuration(bar); ok {
			params["start"] = strconv.FormatInt(time.Now().Add(-d*time.Duration(limit+2)).Unix(), 10)
		}
	}

	var resp klineResponse
	if err := c.doPublic("/api/v1/contract/kline/"+instID, params, &resp); err != nil {
		return nil, fmt.Errorf("mexc get candles %s %s: %w", instID, bar, err)
	}
	candles, err := resp.toCandles()
	if err != nil {
		return nil, fmt.Errorf("mexc get candles %s %s: %w", instID, bar, err)
	}
	// Trim to the requested count, keeping the NEWEST candles — the start-time window above is
	// deliberately generous, so an over-long result is expected rather than exceptional.
	if limit > 0 && len(candles) > limit {
		candles = candles[len(candles)-limit:]
	}
	return candles, nil
}

// contractDetail is /api/v1/contract/detail's data object — the contract-shape metadata required to
// convert a notional size into a valid contract count before placing an order.
//
// Getting this wrong is not cosmetic: CLAUDE.md §33.3 records that assuming a contract multiplier
// of 1 would have sized every real order on OKX's X-Perp instrument ~10,000x too large. MEXC's
// BTC_USDT has contractSize 0.0001, so the same care applies here.
type contractDetail struct {
	Symbol       string      `json:"symbol"`
	BaseCoin     string      `json:"baseCoin"`
	ContractSize json.Number `json:"contractSize"`
	VolUnit      json.Number `json:"volUnit"`
	MinVol       json.Number `json:"minVol"`
	PriceUnit    json.Number `json:"priceUnit"`
	MaxLeverage  int         `json:"maxLeverage"`
}

// GetInstrument fetches one instrument's contract shape.
//
// instType is accepted and ignored: MEXC has a single futures product per symbol, with no analogue
// to OKX's SWAP/FUTURES/X-Perp split (§33.2). The parameter stays in the signature because it is
// part of port.ExchangeClient, and silently ignoring it here is correct — inventing a MEXC
// "instType" concept to honour it would import an OKX problem this exchange does not have.
func (c *Client) GetInstrument(instType, instID string) (domain.Instrument, error) {
	var resp contractDetail
	if err := c.doPublic("/api/v1/contract/detail", map[string]string{"symbol": instID}, &resp); err != nil {
		return domain.Instrument{}, fmt.Errorf("mexc get instrument %s: %w", instID, err)
	}
	return domain.Instrument{
		InstID:   resp.Symbol,
		CtVal:    num(resp.ContractSize),
		LotSz:    num(resp.VolUnit),
		MinSz:    num(resp.MinVol),
		CtValCcy: resp.BaseCoin,
		TickSz:   num(resp.PriceUnit),
	}, nil
}

// fundingRateHistory is /api/v1/contract/funding_rate/history's data object.
type fundingRateHistory struct {
	ResultList []struct {
		Symbol      string      `json:"symbol"`
		FundingRate json.Number `json:"fundingRate"`
		SettleTime  int64       `json:"settleTime"`
	} `json:"resultList"`
}

// GetFundingRateHistory fetches recent settled funding periods, oldest-first.
//
// MEXC returns them newest-first (verified live), so they are reversed here to match the port's
// documented oldest-first contract — the same order the OKX adapter returns. A caller must never
// have to ask which exchange produced a slice to know how to read it.
func (c *Client) GetFundingRateHistory(instID string, limit int) ([]domain.FundingRate, error) {
	if limit <= 0 {
		limit = 100
	}
	var resp fundingRateHistory
	params := map[string]string{
		"symbol":    instID,
		"page_num":  "1",
		"page_size": strconv.Itoa(limit),
	}
	if err := c.doPublic("/api/v1/contract/funding_rate/history", params, &resp); err != nil {
		return nil, fmt.Errorf("mexc get funding rate history %s: %w", instID, err)
	}
	out := make([]domain.FundingRate, 0, len(resp.ResultList))
	for i := len(resp.ResultList) - 1; i >= 0; i-- {
		r := resp.ResultList[i]
		out = append(out, domain.FundingRate{
			InstID:      r.Symbol,
			FundingRate: num(r.FundingRate),
			// settleTime is MILLISECONDS here, unlike kline's seconds — verified live. The two
			// endpoints genuinely disagree, so each conversion is written against what that
			// endpoint actually sends rather than a package-wide assumption.
			FundingTime: time.UnixMilli(r.SettleTime).UTC(),
		})
	}
	return out, nil
}

// num converts a JSON number's literal text to a decimal.
//
// Built from the text rather than a float64 so the digits MEXC sent are the digits stored — the
// whole reason this project uses decimal.Decimal (§14). An unparseable value yields zero rather
// than an error: these come from a response that already passed envelope validation, and a single
// malformed field must not discard an otherwise good ticker. A zero price is caught by the
// positivity checks the trading paths already apply.
func num(n json.Number) decimal.Decimal {
	if n == "" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(n.String())
	if err != nil {
		return decimal.Zero
	}
	return d
}
