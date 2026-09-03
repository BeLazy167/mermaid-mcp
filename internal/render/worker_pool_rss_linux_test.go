//go:build linux

package render

import (
	"context"
	"testing"
)

func TestWorkerPoolRecyclesWhenProcessGroupRSSExceedsLimit(t *testing.T) {
	config := testWorkerPoolConfig()
	config.MaxWorkerRSSBytes = 1
	pool, stateDir := newTestWorkerPool(t, config)
	defer closeWorkerPool(t, pool)

	result, err := pool.Render(context.Background(), Request{Diagram: "graph TD; A-->B", Format: FormatSVG})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if err := result.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if starts := readWorkerStarts(t, stateDir); len(starts) < 2 {
		t.Fatalf("worker starts = %v, want RSS-triggered replacement", starts)
	}
}
