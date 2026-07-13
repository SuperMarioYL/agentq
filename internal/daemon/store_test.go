package daemon

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStore_PutGetEnvelope(t *testing.T) {
	s := newTestStore(t)
	env := &protocol.ApprovalEnvelope{
		ID: "01ABC", AgentID: "a", Prompt: "ok?",
		Choices:   []protocol.Choice{{Key: "y", Label: "Approve", IsDefault: true}},
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	if err := s.PutEnvelope(env); err != nil {
		t.Fatalf("PutEnvelope: %v", err)
	}
	got, err := s.GetEnvelope("01ABC")
	if err != nil {
		t.Fatalf("GetEnvelope: %v", err)
	}
	if got.AgentID != "a" || len(got.Choices) != 1 || got.Choices[0].Key != "y" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestStore_DeleteEnvelope(t *testing.T) {
	s := newTestStore(t)
	env := &protocol.ApprovalEnvelope{
		ID: "del-1", AgentID: "a", Prompt: "ok?",
		Choices: []protocol.Choice{{Key: "y", Label: "Approve", IsDefault: true}},
	}
	if err := s.PutEnvelope(env); err != nil {
		t.Fatalf("PutEnvelope: %v", err)
	}
	if err := s.DeleteEnvelope("del-1"); err != nil {
		t.Fatalf("DeleteEnvelope: %v", err)
	}
	if _, err := s.GetEnvelope("del-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetEnvelope after delete err=%v want ErrNotFound", err)
	}
	// Idempotent: deleting an absent ID is a no-op, not an error.
	if err := s.DeleteEnvelope("del-1"); err != nil {
		t.Errorf("second DeleteEnvelope err=%v want nil (idempotent)", err)
	}
	// An empty ID is rejected.
	if err := s.DeleteEnvelope(""); err == nil {
		t.Error("DeleteEnvelope(\"\") err=nil want error")
	}
}

func TestStore_GetMissing(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetEnvelope("none")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err=%v want ErrNotFound", err)
	}
	_, err = s.GetAnswer("none")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("answer err=%v want ErrNotFound", err)
	}
}

func TestStore_PutEnvelopeMissingID(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutEnvelope(&protocol.ApprovalEnvelope{}); err == nil {
		t.Fatal("expected error on empty ID")
	}
	if err := s.PutEnvelope(nil); err == nil {
		t.Fatal("expected error on nil envelope")
	}
}

func TestStore_PutAnswerMissingID(t *testing.T) {
	s := newTestStore(t)
	if err := s.PutAnswer(&protocol.Answer{}); err == nil {
		t.Fatal("expected error on empty envelope_id")
	}
	if err := s.PutAnswer(nil); err == nil {
		t.Fatal("expected error on nil answer")
	}
}

func TestStore_ListSkipsAnswered(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"01", "02", "03"} {
		if err := s.PutEnvelope(&protocol.ApprovalEnvelope{ID: id, AgentID: "a", Prompt: "p"}); err != nil {
			t.Fatalf("PutEnvelope %s: %v", id, err)
		}
	}
	if err := s.PutAnswer(&protocol.Answer{
		EnvelopeID: "02", ChoiceKey: "y", AnsweredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("PutAnswer: %v", err)
	}
	list, err := s.ListEnvelopes(10)
	if err != nil {
		t.Fatalf("ListEnvelopes: %v", err)
	}
	if len(list) != 2 || list[0].ID != "01" || list[1].ID != "03" {
		t.Errorf("unexpected list: %+v", list)
	}
}

// TestStore_ListSkipsExpired guards fix-expired-envelopes-linger-in-queue: an
// envelope whose ExpiresAt is in the past must be excluded from ListEnvelopes
// even with no stored answer, while a future-dated and a zero-ExpiresAt envelope
// are kept. Before the fix ListEnvelopes filtered only on a stored answer.
func TestStore_ListSkipsExpired(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	_ = s.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "01future", AgentID: "a", Prompt: "p", ExpiresAt: now.Add(time.Hour),
	})
	_ = s.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "02expired", AgentID: "a", Prompt: "p", ExpiresAt: now.Add(-time.Hour),
	})
	_ = s.PutEnvelope(&protocol.ApprovalEnvelope{
		ID: "03noexpiry", AgentID: "a", Prompt: "p", // zero ExpiresAt = never expires
	})
	list, err := s.ListEnvelopes(10)
	if err != nil {
		t.Fatalf("ListEnvelopes: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("len=%d want 2 (expired dropped): %+v", len(list), list)
	}
	for _, e := range list {
		if e.ID == "02expired" {
			t.Errorf("expired envelope still listed: %+v", e)
		}
	}
}

func TestStore_ListRespectsLimit(t *testing.T) {
	s := newTestStore(t)
	for _, id := range []string{"01", "02", "03"} {
		_ = s.PutEnvelope(&protocol.ApprovalEnvelope{ID: id, AgentID: "a", Prompt: "p"})
	}
	list, err := s.ListEnvelopes(2)
	if err != nil {
		t.Fatalf("ListEnvelopes: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("len=%d want 2", len(list))
	}
}

func TestStore_AnswerRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ans := &protocol.Answer{EnvelopeID: "01", ChoiceKey: "n", AnsweredAt: time.Now().UTC()}
	if err := s.PutAnswer(ans); err != nil {
		t.Fatalf("PutAnswer: %v", err)
	}
	got, err := s.GetAnswer("01")
	if err != nil {
		t.Fatalf("GetAnswer: %v", err)
	}
	if got.ChoiceKey != "n" {
		t.Errorf("ChoiceKey=%q", got.ChoiceKey)
	}
}

func TestStore_PutAnswerIfAbsentIsCreateOnly(t *testing.T) {
	s := newTestStore(t)
	first := &protocol.Answer{EnvelopeID: "01", ChoiceKey: "y", AnsweredAt: time.Now().UTC()}
	stored, err := s.PutAnswerIfAbsent(first)
	if err != nil {
		t.Fatalf("first PutAnswerIfAbsent: %v", err)
	}
	if stored.ChoiceKey != "y" {
		t.Fatalf("first store ChoiceKey=%q want y", stored.ChoiceKey)
	}

	// A second answer to the SAME envelope must not overwrite; it must report
	// ErrAnswerExists and hand back the ORIGINAL answer, not the new one.
	second := &protocol.Answer{EnvelopeID: "01", ChoiceKey: "n", AnsweredAt: time.Now().UTC()}
	got, err := s.PutAnswerIfAbsent(second)
	if !errors.Is(err, ErrAnswerExists) {
		t.Fatalf("second PutAnswerIfAbsent err=%v want ErrAnswerExists", err)
	}
	if got == nil || got.ChoiceKey != "y" {
		t.Fatalf("returned answer=%+v; want the original ChoiceKey=y", got)
	}

	// The persisted record must still be the first choice.
	onDisk, err := s.GetAnswer("01")
	if err != nil {
		t.Fatalf("GetAnswer: %v", err)
	}
	if onDisk.ChoiceKey != "y" {
		t.Errorf("stored answer overwritten: ChoiceKey=%q want y", onDisk.ChoiceKey)
	}
}

func TestStore_PutAnswerIfAbsentMissingID(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.PutAnswerIfAbsent(&protocol.Answer{ChoiceKey: "y"}); err == nil {
		t.Fatal("expected error for answer without envelope_id")
	}
}

func TestStore_ReopenPreservesData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.db")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if err := s.PutEnvelope(&protocol.ApprovalEnvelope{ID: "01", AgentID: "a", Prompt: "p"}); err != nil {
		t.Fatalf("PutEnvelope: %v", err)
	}
	_ = s.Close()
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetEnvelope("01"); err != nil {
		t.Errorf("data lost across reopen: %v", err)
	}
}
