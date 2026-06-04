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
