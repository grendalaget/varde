package main

import (
	"sync"

	meshv1 "github.com/grendalaget/varde/go/gen/mesh/v1"
)

// eventBus fans MeshEvents out to WatchEvents subscribers.
type eventBus struct {
	mu   sync.Mutex
	subs map[chan *meshv1.MeshEvent]struct{}
}

func newEventBus() *eventBus {
	return &eventBus{subs: map[chan *meshv1.MeshEvent]struct{}{}}
}

func (b *eventBus) subscribe() chan *meshv1.MeshEvent {
	ch := make(chan *meshv1.MeshEvent, 128)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *eventBus) unsubscribe(ch chan *meshv1.MeshEvent) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

func (b *eventBus) emit(ev *meshv1.MeshEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}
