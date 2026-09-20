package okx

import (
	"testing"
)

// TestBookMerger_ChecksumMatchesOKXAlgorithm proves the checksum formula (interleave top-25
// bid/ask as "price:size" pairs, CRC32-IEEE, reinterpret as signed int32) against a hand-computed
// worked example — verified independently in Python (zlib.crc32) rather than trusted by inspection,
// since a wrong checksum would make this code reject every real book as "corrupt" and resubscribe
// forever (docs/MANUAL_TRADE_PLAN.md §7's checksum requirement). Live OKX traffic captured while
// building this (BTC-USDT-SWAP/ETH-USDT-SWAP `books` channel) always sent checksum=0 on this
// account/host, so this synthetic fixture is what actually exercises the non-degenerate path.
func TestBookMerger_ChecksumMatchesOKXAlgorithm(t *testing.T) {
	m := NewBookMerger()
	// Python: zlib.crc32(b"0.0683:20:0.0684:10") = 3911574963 -> signed int32 -383392333
	const wantChecksum = int32(-383392333)

	err := m.Apply("snapshot", BooksPush{
		Bids:     []WireLevel{{"0.0683", "20", "0", "1"}},
		Asks:     []WireLevel{{"0.0684", "10", "0", "1"}},
		Checksum: wantChecksum,
	})
	if err != nil {
		t.Fatalf("Apply with the correct checksum should succeed, got: %v", err)
	}
}

// TestBookMerger_RejectsAMismatchedChecksum confirms a wrong checksum is a real error, not
// silently ignored — the exact "do not silently serve a possibly-corrupt book" requirement.
func TestBookMerger_RejectsAMismatchedChecksum(t *testing.T) {
	m := NewBookMerger()
	err := m.Apply("snapshot", BooksPush{
		Bids:     []WireLevel{{"0.0683", "20", "0", "1"}},
		Asks:     []WireLevel{{"0.0684", "10", "0", "1"}},
		Checksum: 12345, // deliberately wrong
	})
	if err == nil {
		t.Fatal("expected a checksum mismatch error, got nil")
	}
}

// A checksum of exactly 0 is treated as "not provided" rather than validated — live-verified OKX
// traffic on this account/host always sends 0 for the `books` channel, and treating that as a real
// checksum to match against would make every snapshot fail.
func TestBookMerger_ZeroChecksumSkipsValidation(t *testing.T) {
	m := NewBookMerger()
	err := m.Apply("snapshot", BooksPush{
		Bids:     []WireLevel{{"0.0683", "20", "0", "1"}},
		Asks:     []WireLevel{{"0.0684", "10", "0", "1"}},
		Checksum: 0,
	})
	if err != nil {
		t.Fatalf("a zero checksum must not be validated, got error: %v", err)
	}
}

// TestBookMerger_UpdateBeforeSnapshotIsAnError guards the merge's own precondition — an update has
// no book to apply deltas against until a snapshot has arrived at least once.
func TestBookMerger_UpdateBeforeSnapshotIsAnError(t *testing.T) {
	m := NewBookMerger()
	err := m.Apply("update", BooksPush{Bids: []WireLevel{{"1", "1", "0", "1"}}})
	if err == nil {
		t.Fatal("expected an error applying an update with no prior snapshot")
	}
}

// TestBookMerger_UpdateUpsertsAndRemoves confirms the delta semantics: a size of "0" removes a
// level, anything else upserts it — OKX's own documented `books` channel behavior.
func TestBookMerger_UpdateUpsertsAndRemoves(t *testing.T) {
	m := NewBookMerger()
	if err := m.Apply("snapshot", BooksPush{
		Asks: []WireLevel{{"100", "1", "0", "1"}, {"101", "2", "0", "1"}},
		Bids: []WireLevel{{"99", "1", "0", "1"}},
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Remove ask@100, upsert ask@101's size, add a new bid.
	if err := m.Apply("update", BooksPush{
		Asks: []WireLevel{{"100", "0", "0", "0"}, {"101", "5", "0", "2"}},
		Bids: []WireLevel{{"98", "3", "0", "1"}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}

	asks, bids := m.TopN(10)
	if len(asks) != 1 || asks[0].Px != "101" || asks[0].Sz != "5" {
		t.Fatalf("unexpected asks after update: %+v", asks)
	}
	if len(bids) != 2 {
		t.Fatalf("unexpected bids after update: %+v", bids)
	}
}

// TestBookMerger_TopNOrdersCorrectly confirms asks come back ascending (best/lowest first) and
// bids descending (best/highest first) — every consumer of this data assumes that convention.
func TestBookMerger_TopNOrdersCorrectly(t *testing.T) {
	m := NewBookMerger()
	if err := m.Apply("snapshot", BooksPush{
		Asks: []WireLevel{{"103", "1", "0", "1"}, {"101", "1", "0", "1"}, {"102", "1", "0", "1"}},
		Bids: []WireLevel{{"97", "1", "0", "1"}, {"99", "1", "0", "1"}, {"98", "1", "0", "1"}},
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	asks, bids := m.TopN(2)
	if len(asks) != 2 || asks[0].Px != "101" || asks[1].Px != "102" {
		t.Fatalf("asks not ascending/truncated correctly: %+v", asks)
	}
	if len(bids) != 2 || bids[0].Px != "99" || bids[1].Px != "98" {
		t.Fatalf("bids not descending/truncated correctly: %+v", bids)
	}
}

// TestBookMerger_TopNBeforeAnySnapshotReturnsNothing confirms a merger that has received no
// snapshot yet reports an empty book rather than panicking on a nil map.
func TestBookMerger_TopNBeforeAnySnapshotReturnsNothing(t *testing.T) {
	m := NewBookMerger()
	asks, bids := m.TopN(8)
	if asks != nil || bids != nil {
		t.Fatalf("expected nil/nil before any snapshot, got asks=%v bids=%v", asks, bids)
	}
}
