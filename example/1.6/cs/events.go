package main

import (
	"sync"
	"time"
)

// hyde/lab: minimal in-process event bus behind the SSE stream. This is the
// lab stand-in for the CommandRouter/event-publisher seam (ADR-0002): the
// browser-facing contract (event names + JSON payloads) is what survives —
// at M3 the publisher swaps to NATS JetStream without touching consumers.

// Event is one registry/protocol occurrence, streamed to SSE subscribers.
type Event struct {
	Type    string         `json:"type"`
	Charger string         `json:"charger"`
	Time    time.Time      `json:"time"`
	Data    map[string]any `json:"data,omitempty"`
}

const busRingSize = 100

type eventBus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
	ring []Event // last busRingSize events, oldest first
}

var bus = &eventBus{subs: map[chan Event]struct{}{}}

// Publish fans out to all subscribers (non-blocking: slow subscribers drop)
// and records the event in the replay ring.
func (b *eventBus) Publish(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ring = append(b.ring, ev)
	if len(b.ring) > busRingSize {
		b.ring = b.ring[len(b.ring)-busRingSize:]
	}
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // subscriber too slow — drop rather than block the WS path
		}
	}
}

// Subscribe returns a buffered event channel plus an unsubscribe func.
func (b *eventBus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

// Replay returns up to n most recent events, oldest first.
func (b *eventBus) Replay(n int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n > len(b.ring) {
		n = len(b.ring)
	}
	out := make([]Event, n)
	copy(out, b.ring[len(b.ring)-n:])
	return out
}
