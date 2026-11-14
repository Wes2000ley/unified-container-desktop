package engine

import "testing"

func TestActivityWaitsForDetachedStreams(t *testing.T) {
	streamDone, transferDone := make(chan struct{}), make(chan struct{})
	b := &Backend{
		sessions: map[string]*execSession{"build": {finished: streamDone}},
		transfers: map[string]*transferSession{
			"upload":   {done: transferDone},
			"download": {}, // Reader-only transfers stay active until closed.
		},
	}
	if got := b.activeOperations(); got != 3 {
		t.Fatalf("unfinished activity = %d, want 3", got)
	}
	close(streamDone)
	close(transferDone)
	if got := b.activeOperations(); got != 1 {
		t.Fatalf("completed producers still prevent sleep: %d", got)
	}
	delete(b.transfers, "download")
	if got := b.activeOperations(); got != 0 {
		t.Fatalf("closed transfers still prevent sleep: %d", got)
	}
}
