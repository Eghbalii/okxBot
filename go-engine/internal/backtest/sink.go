package backtest

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sync"
)

// JSONLSink streams samples to a file, one JSON object per line.
//
// JSONL rather than one JSON array: a run over months of history across every registered strategy
// produces tens of thousands of samples, and an array must be complete before it can be parsed —
// so an interrupted run would yield a file nothing can read. Line-delimited means a partial file is
// still a usable dataset up to the point it stopped, which matters when a run takes minutes and the
// alternative to salvaging it is starting over.
type JSONLSink struct {
	mu sync.Mutex
	w  *bufio.Writer
	c  io.Closer
	n  int
}

// NewJSONLSink creates or truncates path.
func NewJSONLSink(path string) (*JSONLSink, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &JSONLSink{w: bufio.NewWriterSize(f, 1<<20), c: f}, nil
}

func (s *JSONLSink) Write(sample Sample) error {
	b, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		return err
	}
	s.n++
	return nil
}

// Count reports how many samples were written.
func (s *JSONLSink) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// Close flushes and closes. MUST be called: a buffered writer silently drops up to a megabyte of
// samples otherwise, and a dataset short by its last thousand trades looks exactly like one that
// simply ended there.
func (s *JSONLSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.w.Flush(); err != nil {
		return err
	}
	return s.c.Close()
}

// MemorySink collects samples in memory, for tests.
type MemorySink struct {
	mu      sync.Mutex
	Samples []Sample
}

func (m *MemorySink) Write(s Sample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Samples = append(m.Samples, s)
	return nil
}

// DiscardSink counts samples without storing them, for a screening run.
//
// Scoring 26 strategies to decide which few to keep produces a 125MB file that is discarded the
// moment the answer is read — and writing it is most of the run's wall time.
type DiscardSink struct{}

func (DiscardSink) Write(Sample) error { return nil }
