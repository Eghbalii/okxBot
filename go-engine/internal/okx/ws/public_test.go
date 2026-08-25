package ws

import (
	"encoding/json"
	"testing"
)

// Regression test: OKX's subscribe-ack frames echo "arg" (so Arg.Channel is non-empty) but carry
// no "data" field. Treating "Arg.Channel != """ alone as "this is a data push" causes every ack
// frame to fail json.Unmarshal into []json.RawMessage downstream with "unexpected end of JSON
// input" — confirmed against live OKX WS payloads during the paper-trading dry run.
func TestMessage_IsDataPush(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "subscribe ack",
			raw:  `{"event":"subscribe","arg":{"channel":"tickers","instId":"BTC-USDT-SWAP"},"connId":"9128911c"}`,
			want: false,
		},
		{
			name: "error ack",
			raw:  `{"event":"error","code":"60012","msg":"invalid request","connId":"9128911c"}`,
			want: false,
		},
		{
			name: "ticker data push",
			raw:  `{"arg":{"channel":"tickers","instId":"BTC-USDT-SWAP"},"data":[{"instId":"BTC-USDT-SWAP","last":"79028.5"}]}`,
			want: true,
		},
		{
			name: "candle data push",
			raw:  `{"arg":{"channel":"candle1m","instId":"BTC-USDT-SWAP"},"data":[["1700000000000","100","101","99","100.5","10"]]}`,
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var msg Message
			if err := json.Unmarshal([]byte(tc.raw), &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got := msg.isDataPush(); got != tc.want {
				t.Errorf("isDataPush() = %v, want %v", got, tc.want)
			}
			if tc.want {
				var arr []json.RawMessage
				if err := json.Unmarshal(msg.Data, &arr); err != nil {
					t.Errorf("expected msg.Data to decode as a non-empty array for a data push, got error: %v", err)
				}
			}
		})
	}
}
