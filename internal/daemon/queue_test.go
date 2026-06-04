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
