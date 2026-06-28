package daemon

import (
	"context"
	"errors"
	"sync"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

// Event is the unit broadcast on the WebSocket. Exactly one of Envelope
// or Answer is non-nil depending on Kind.
type Event struct {
	Kind     string                     `json:"kind"`
	Envelope *protocol.ApprovalEnvelope `json:"envelope,omitempty"`
	Answer   *protocol.Answer           `json:"answer,omitempty"`
}

// Event kinds carried on the WebSocket.
const (
	EventNewEnvelope = "envelope"
	EventAnswered    = "answer"
)

// ErrAlreadyAnswered is returned when Answer is called twice for the
// same envelope ID without an intervening Wait drain.
var ErrAlreadyAnswered = errors.New("daemon: envelope already answered")

// Queue tracks in-flight envelope→waiter channels so the daemon can
// deliver an Answer back to the producing wrapper that called Register,
// and runs a small fan-out hub for WebSocket subscribers.
//
// State is intentionally in-memory only: an Answer that arrives after
// the wrapper times out is still persisted by the server, but the
// Queue itself does not retain it.
type Queue struct {
	mu          sync.Mutex
	waiters     map[string]chan protocol.Answer
	subscribers map[chan Event]struct{}
}

// NewQueue returns an empty Queue.
func NewQueue() *Queue {
	return &Queue{
		waiters:     make(map[string]chan protocol.Answer),
		subscribers: make(map[chan Event]struct{}),
	}
}

// Register reserves a waiter slot for env.ID and broadcasts an
// EventNewEnvelope to subscribers. Call Wait afterwards to block on
// the answer.
func (q *Queue) Register(env *protocol.ApprovalEnvelope) error {
	if env == nil || env.ID == "" {
		return errors.New("daemon: register envelope without id")
	}
	q.mu.Lock()
	if _, exists := q.waiters[env.ID]; exists {
		q.mu.Unlock()
		return errors.New("daemon: envelope already registered")
	}
	q.waiters[env.ID] = make(chan protocol.Answer, 1)
	q.mu.Unlock()
	q.broadcast(Event{Kind: EventNewEnvelope, Envelope: env})
	return nil
}

// Wait blocks until either an Answer arrives via Answer or ctx is
// cancelled. The waiter slot is released either way so a slow producer
// cannot leak entries.
//
// On ctx cancellation the slot is removed under q.mu and the channel is
// drained once: a concurrent Answer either delivered before the delete
// (so the buffered value is returned and reported as answered) or has not
// yet looked the waiter up (so it will find the slot gone and return
// ErrNotFound). This closes the race where an answer was buffered into a
// released slot, leaving the HTTP caller a false 200 while the wrapper
// timed out.
func (q *Queue) Wait(ctx context.Context, id string) (protocol.Answer, error) {
	q.mu.Lock()
	ch, ok := q.waiters[id]
	q.mu.Unlock()
	if !ok {
		return protocol.Answer{}, ErrNotFound
	}
	select {
	case ans := <-ch:
		q.release(id)
		return ans, nil
	case <-ctx.Done():
		// Remove the slot under the lock so Answer can no longer deliver
		// into it. If Answer already buffered a value (delivered before we
		// grabbed the lock), honor it instead of dropping it on the floor.
		q.mu.Lock()
		delete(q.waiters, id)
		q.mu.Unlock()
		select {
		case ans := <-ch:
			return ans, nil
		default:
			return protocol.Answer{}, ctx.Err()
		}
	}
}

// Answer delivers ans to the waiter registered for ans.EnvelopeID and
// broadcasts an EventAnswered. Returns ErrNotFound if no waiter exists
// (wrapper already gave up / already drained on timeout) or
// ErrAlreadyAnswered if the slot was already filled.
//
// The lookup, the channel send, and the EventAnswered broadcast all happen
// under q.mu so they are atomic with respect to Wait's timeout path: once
// Wait has removed the slot, Answer sees ok == false and returns
// ErrNotFound rather than buffering an answer nobody will ever read.
func (q *Queue) Answer(ans protocol.Answer) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	ch, ok := q.waiters[ans.EnvelopeID]
	if !ok {
		return ErrNotFound
	}
	select {
	case ch <- ans:
		q.broadcastLocked(Event{Kind: EventAnswered, Answer: &ans})
		return nil
	default:
		return ErrAlreadyAnswered
	}
}

// Pending reports whether a waiter is still registered for id. Used by
// tests and the /api/queue endpoint to distinguish "still waiting" from
// "wrapper gave up".
func (q *Queue) Pending(id string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, ok := q.waiters[id]
	return ok
}

func (q *Queue) release(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.waiters, id)
}

// Subscribe returns a channel that receives every broadcast Event until
// the returned cancel is called. The channel is buffered; slow
// subscribers drop events rather than blocking the producer (the WS
// handler resyncs via /api/queue on reconnect).
func (q *Queue) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	q.mu.Lock()
	q.subscribers[ch] = struct{}{}
	q.mu.Unlock()
	cancel := func() {
		q.mu.Lock()
		if _, ok := q.subscribers[ch]; ok {
			delete(q.subscribers, ch)
			close(ch)
		}
		q.mu.Unlock()
	}
	return ch, cancel
}

func (q *Queue) broadcast(ev Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.broadcastLocked(ev)
}

// broadcastLocked fans ev out to subscribers. The caller MUST already hold
// q.mu — Answer broadcasts under the lock so delivery and the answered
// event are atomic with respect to Wait's timeout path.
func (q *Queue) broadcastLocked(ev Event) {
	for ch := range q.subscribers {
		select {
		case ch <- ev:
		default:
			// Subscriber is slow; better to drop than block the producer.
		}
	}
}
