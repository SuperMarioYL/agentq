package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

func TestQueue_RegisterAndAnswer(t *testing.T) {
	q := NewQueue()
	env := &protocol.ApprovalEnvelope{ID: "01ABC", AgentID: "a", Prompt: "p"}
	if err := q.Register(env); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !q.Pending(env.ID) {
		t.Fatal("expected pending immediately after Register")
	}
	done := make(chan protocol.Answer, 1)
	go func() {
		ans, err := q.Wait(context.Background(), env.ID)
		if err != nil {
			t.Errorf("Wait: %v", err)
		}
		done <- ans
	}()
	// Give Wait a moment to attach to the channel before we Answer.
	time.Sleep(10 * time.Millisecond)
	if err := q.Answer(protocol.Answer{
		EnvelopeID: env.ID,
		ChoiceKey:  "y",
		AnsweredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	select {
	case ans := <-done:
		if ans.ChoiceKey != "y" {
			t.Errorf("ChoiceKey=%q want y", ans.ChoiceKey)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not return after Answer")
	}
	if q.Pending(env.ID) {
		t.Fatal("expected not pending after Wait drained")
	}
}

func TestQueue_DuplicateRegisterFails(t *testing.T) {
	q := NewQueue()
	env := &protocol.ApprovalEnvelope{ID: "01"}
	if err := q.Register(env); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := q.Register(env); err == nil {
		t.Fatal("expected duplicate-register error")
	}
}

func TestQueue_RegisterRejectsEmptyID(t *testing.T) {
	q := NewQueue()
	if err := q.Register(&protocol.ApprovalEnvelope{}); err == nil {
		t.Fatal("expected error on empty ID")
	}
	if err := q.Register(nil); err == nil {
		t.Fatal("expected error on nil envelope")
	}
}

func TestQueue_AnswerWithoutWait(t *testing.T) {
	q := NewQueue()
	err := q.Answer(protocol.Answer{EnvelopeID: "missing"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

func TestQueue_AnswerTwiceWithoutWaitIsRejected(t *testing.T) {
	q := NewQueue()
	env := &protocol.ApprovalEnvelope{ID: "01"}
	if err := q.Register(env); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := q.Answer(protocol.Answer{EnvelopeID: env.ID, ChoiceKey: "y"}); err != nil {
		t.Fatalf("first Answer: %v", err)
	}
	// No Wait consumed the channel; the second Answer should be rejected
	// because the 1-buffer slot is still occupied.
	if err := q.Answer(protocol.Answer{EnvelopeID: env.ID, ChoiceKey: "y"}); err != ErrAlreadyAnswered {
		t.Fatalf("second Answer err=%v want ErrAlreadyAnswered", err)
	}
}

func TestQueue_WaitCanceled(t *testing.T) {
	q := NewQueue()
	env := &protocol.ApprovalEnvelope{ID: "01"}
	_ = q.Register(env)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Wait(ctx, env.ID); err == nil {
		t.Fatal("expected cancellation error")
	}
	if q.Pending(env.ID) {
		t.Fatal("expected waiter released after cancellation")
	}
}

func TestQueue_WaitUnknownID(t *testing.T) {
	q := NewQueue()
	if _, err := q.Wait(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v want ErrNotFound", err)
	}
}

// TestQueue_AnswerAfterTimeoutReturnsNotFound guards the lost-answer race:
// once Wait has timed out and released the slot, a late Answer must report
// ErrNotFound so the HTTP layer replies 202 (persisted-for-audit) instead of
// a false 200. Previously Answer buffered into the orphaned channel and
// returned nil, telling the phone the approval landed while the wrapper had
// already aborted with a 504.
func TestQueue_AnswerAfterTimeoutReturnsNotFound(t *testing.T) {
	q := NewQueue()
	env := &protocol.ApprovalEnvelope{ID: "01RACE", AgentID: "a", Prompt: "p"}
	if err := q.Register(env); err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Wait(ctx, env.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait err=%v want context.Canceled", err)
	}
	if q.Pending(env.ID) {
		t.Fatal("expected waiter released after timeout")
	}
	// The late answer must not be silently buffered into a dead slot.
	err := q.Answer(protocol.Answer{EnvelopeID: env.ID, ChoiceKey: "y"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Answer err=%v want ErrNotFound (raced past Wait timeout)", err)
	}
}

// TestQueue_AnswerBufferedBeforeTimeoutIsHonored covers the other side of the
// race: an Answer that delivered into the channel just before ctx fired must
// still be returned to the caller, not dropped, so a real approval is never
// lost when the timeout and the answer land in the same instant.
func TestQueue_AnswerBufferedBeforeTimeoutIsHonored(t *testing.T) {
	q := NewQueue()
	env := &protocol.ApprovalEnvelope{ID: "01HONOR", AgentID: "a", Prompt: "p"}
	if err := q.Register(env); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Deliver the answer first (buffered into the cap-1 channel), then call
	// Wait with an already-cancelled ctx. Wait must drain the buffered answer
	// rather than reporting the cancellation.
	if err := q.Answer(protocol.Answer{EnvelopeID: env.ID, ChoiceKey: "a"}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ans, err := q.Wait(ctx, env.ID)
	if err != nil {
		t.Fatalf("Wait err=%v want buffered answer honored", err)
	}
	if ans.ChoiceKey != "a" {
		t.Fatalf("ChoiceKey=%q want %q", ans.ChoiceKey, "a")
	}
	if q.Pending(env.ID) {
		t.Fatal("expected waiter released after draining buffered answer")
	}
}

func TestQueue_SubscribeBroadcast(t *testing.T) {
	q := NewQueue()
	ch, cancel := q.Subscribe()
	defer cancel()
	env := &protocol.ApprovalEnvelope{ID: "01", AgentID: "a", Prompt: "p"}
	if err := q.Register(env); err != nil {
		t.Fatalf("Register: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Kind != EventNewEnvelope || ev.Envelope == nil || ev.Envelope.ID != env.ID {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no broadcast received")
	}
	// Answer should also broadcast.
	go func() { _, _ = q.Wait(context.Background(), env.ID) }()
	time.Sleep(10 * time.Millisecond)
	if err := q.Answer(protocol.Answer{EnvelopeID: env.ID, ChoiceKey: "y"}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	select {
	case ev := <-ch:
		if ev.Kind != EventAnswered || ev.Answer == nil || ev.Answer.ChoiceKey != "y" {
			t.Errorf("unexpected answered event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no answered broadcast")
	}
}

func TestQueue_SubscribeCancelClosesChannel(t *testing.T) {
	q := NewQueue()
	ch, cancel := q.Subscribe()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("expected channel closed after cancel")
	}
}

// TestQueue_BroadcastAnsweredWithoutWaiter guards fix-answered-card-not-broadcast-to-other-tabs:
// the 202 and 409 answer paths resolve a card that has NO live waiter, so
// Queue.Answer's success branch never fires the EventAnswered. BroadcastAnswered
// must still fan an answered event out to every subscriber so other phones drop
// the dead card. Before the fix there was no such helper and those paths were
// silent.
func TestQueue_BroadcastAnsweredWithoutWaiter(t *testing.T) {
	q := NewQueue()
	ch, cancel := q.Subscribe()
	defer cancel()

	// No Register / Wait: there is deliberately no waiter, mirroring the 202/409
	// server paths.
	q.BroadcastAnswered(protocol.Answer{EnvelopeID: "01DEAD", ChoiceKey: "y"})

	select {
	case ev := <-ch:
		if ev.Kind != EventAnswered || ev.Answer == nil || ev.Answer.EnvelopeID != "01DEAD" {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("BroadcastAnswered did not reach subscriber")
	}
}

// TestQueue_BroadcastRemoved guards the expiry-removal broadcast used on the
// postEnvelope timeout path: a removal must reach subscribers as an EventAnswered
// (removal) carrying the envelope id so the UI drops the card.
func TestQueue_BroadcastRemoved(t *testing.T) {
	q := NewQueue()
	ch, cancel := q.Subscribe()
	defer cancel()

	q.BroadcastRemoved("01GONE")

	select {
	case ev := <-ch:
		if ev.Kind != EventAnswered || ev.Answer == nil || ev.Answer.EnvelopeID != "01GONE" {
			t.Errorf("unexpected event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("BroadcastRemoved did not reach subscriber")
	}
}
