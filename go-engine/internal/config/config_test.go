package config

import "testing"

func TestValidatePaperTradingBars(t *testing.T) {
	cases := []struct {
		name       string
		ingestion  []string
		paperTrade []string
		wantErr    bool
	}{
		{
			name:       "proper subset passes",
			ingestion:  []string{"1m", "3m", "5m", "1H", "4H", "1D"},
			paperTrade: []string{"1m"},
			wantErr:    false,
		},
		{
			name:       "equal sets pass",
			ingestion:  []string{"1m", "1H"},
			paperTrade: []string{"1m", "1H"},
			wantErr:    false,
		},
		{
			name:       "empty paper trading bars passes trivially",
			ingestion:  []string{"1m"},
			paperTrade: nil,
			wantErr:    false,
		},
		{
			name:       "bar missing from ingestion fails",
			ingestion:  []string{"1m", "1H"},
			paperTrade: []string{"1m", "15m"},
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{}
			cfg.Ingestion.Bars = tc.ingestion
			cfg.PaperTrading.Bars = tc.paperTrade

			err := cfg.ValidatePaperTradingBars()
			if tc.wantErr && err == nil {
				t.Errorf("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("expected no error, got: %v", err)
			}
		})
	}
}
