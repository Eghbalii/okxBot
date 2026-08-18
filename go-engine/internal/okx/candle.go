package okx

// Candle is one OHLCV bar from OKX's REST /market/candles (or WS candle channel), still as the
// raw string fields OKX returns (parse with strconv where needed).
type Candle struct {
	Ts      string // ms epoch timestamp, candle open time
	Open    string
	High    string
	Low     string
	Close   string
	Vol     string
	Confirm string // "0" = still forming, "1" = finalized
}
