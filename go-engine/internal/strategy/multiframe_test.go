package strategy

import (
	"testing"

	"github.com/shopspring/decimal"
)

// singleTFStub implements only Strategy — the existing 14 built-ins' shape.
type singleTFStub struct{ saw int }

func (s *singleTFStub) Name() string                                   { return "single" }
func (s *singleTFStub) Params() []ParamSpec                            { return nil }
func (s *singleTFStub) WithParams(map[string]decimal.Decimal) Strategy { return s }
func (s *singleTFStub) Evaluate(c []Candle) (Signal, error) {
	s.saw = len(c)
	return Signal{Side: Buy}, nil
}

// multiTFStub opts into the higher-timeframe capability.
type multiTFStub struct {
	sawBar    string
	sawHigher int
	higherOK  bool
}

func (s *multiTFStub) Name() string                                   { return "multi" }
func (s *multiTFStub) Params() []ParamSpec                            { return nil }
func (s *multiTFStub) WithParams(map[string]decimal.Decimal) Strategy { return s }
func (s *multiTFStub) Evaluate([]Candle) (Signal, error) {
	return Signal{}, nil // must not be reached when EvaluateView exists
}
func (s *multiTFStub) EvaluateView(v MarketView) (Signal, error) {
	s.sawBar = v.Bar
	h, ok := v.Higher("1H", 2)
	s.higherOK, s.sawHigher = ok, len(h)
	return Signal{Side: Sell}, nil
}

func view() MarketView {
	bars := map[string][]Candle{
		"5m": {{}, {}, {}},
		"1H": {{}, {}},
	}
	return MarketView{Bar: "5m", Candles: bars["5m"], Bars: bars}
}

// A plain Strategy keeps working untouched — the whole reason this is an optional interface rather
// than a change to Evaluate's signature.
func TestEvaluateWith_FallsBackForSingleTimeframeStrategy(t *testing.T) {
	s := &singleTFStub{}
	sig, err := EvaluateWith(s, view())
	if err != nil {
		t.Fatalf("EvaluateWith: %v", err)
	}
	if sig.Side != Buy {
		t.Errorf("want Buy from the fallback path, got %q", sig.Side)
	}
	if s.saw != 3 {
		t.Errorf("want the assigned bar's 3 candles, got %d", s.saw)
	}
}

func TestEvaluateWith_UsesViewForMultiTimeframeStrategy(t *testing.T) {
	s := &multiTFStub{}
	sig, err := EvaluateWith(s, view())
	if err != nil {
		t.Fatalf("EvaluateWith: %v", err)
	}
	if sig.Side != Sell {
		t.Errorf("want Sell from EvaluateView, got %q", sig.Side)
	}
	if s.sawBar != "5m" {
		t.Errorf("want the decision bar 5m, got %q", s.sawBar)
	}
	if !s.higherOK || s.sawHigher != 2 {
		t.Errorf("want 1H context of 2 candles, got ok=%v len=%d", s.higherOK, s.sawHigher)
	}
}

// During warm-up a longer timeframe hasn't filled yet. That must read as "no opinion available",
// not an error, or the engine would stall on every restart.
func TestHigher_ReportsNotOKWhenMissingOrTooShort(t *testing.T) {
	v := view()
	if _, ok := v.Higher("4H", 1); ok {
		t.Error("a bar that isn't collected must report ok=false")
	}
	if _, ok := v.Higher("1H", 5); ok {
		t.Error("a bar with fewer than minLen candles must report ok=false")
	}
	if c, ok := v.Higher("1H", 2); !ok || len(c) != 2 {
		t.Errorf("an available bar must report ok=true, got ok=%v len=%d", ok, len(c))
	}
}
