package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/unix/ugh/internal/llm"
)

func TestQueuedStatusWhileWaitingForProviderSlot(t *testing.T) {
	slot := make(chan struct{}, 1)
	slot <- struct{}{} // another agent holds the only stream
	var mu sync.Mutex
	var statuses []string
	fp := &fakeProvider{turns: [][]llm.Event{answer("ok")}}
	a := New(Config{Provider: fp, Model: "p/m", Cwd: t.TempDir(), StreamLimit: func(string) chan struct{} { return slot },
		OnStatus: func(s string) { mu.Lock(); statuses = append(statuses, s); mu.Unlock() }})
	done := make(chan error, 1)
	go func() { _, err := a.Run(context.Background(), "q"); done <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for a.Node.status() != "queued" {
		if time.Now().After(deadline) {
			t.Fatalf("never queued, status %q", a.Node.status())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(queuedAnnounceAfter + 100*time.Millisecond) // a real wait reaches the session file
	<-slot
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(statuses) != 2 || statuses[0] != "queued" || statuses[1] != "" {
		t.Fatalf("session statuses = %q", statuses)
	}
	if a.Node.status() != "done" {
		t.Fatalf("final status %q", a.Node.status())
	}
}

func TestQueuedStatusRestoredOnCancel(t *testing.T) {
	slot := make(chan struct{}, 1)
	slot <- struct{}{}
	a := New(Config{Provider: &fakeProvider{}, Model: "p/m", Cwd: t.TempDir(), StreamLimit: func(string) chan struct{} { return slot }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := a.Run(ctx, "q"); done <- err }()
	for i := 0; i < 400 && a.Node.status() != "queued"; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected cancellation")
	}
	if got := a.Node.status(); got != "cancelled" {
		t.Fatalf("status after cancel = %q", got)
	}
}
