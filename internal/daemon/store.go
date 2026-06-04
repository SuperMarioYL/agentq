// Package daemon implements the local HTTP+WebSocket service that
// aggregates ApprovalEnvelopes from N wrapped agent sessions into a
// single ordered triage queue.
//
// The package is split into three files:
//
//	store.go   bbolt-backed durable storage keyed by envelope ID
//	queue.go   in-memory waiter map + broadcast hub for live updates
//	server.go  echo router exposing the REST + WebSocket surface
//
// Persistence is intentionally minimal — there is no schema migration,
// no indexes, no retention policy. The store is a queue cache; if the
// daemon dies, in-flight wrappers retry on reconnect.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/SuperMarioYL/agentq/internal/protocol"
)

var (
	envelopesBucket = []byte("envelopes")
	answersBucket   = []byte("answers")
)

// ErrNotFound is returned when an envelope or answer is missing.
var ErrNotFound = errors.New("daemon: not found")

// Store is a bbolt-backed persistence layer for envelopes + answers.
// One file, two buckets — that's the entire schema.
type Store struct {
	db *bolt.DB
}

// OpenStore opens (or creates) the bbolt file at path and ensures the
// envelopes/answers buckets exist.
func OpenStore(path string) (*Store, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("daemon: open store %q: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{envelopesBucket, answersBucket} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("daemon: init buckets: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying bbolt file lock.
func (s *Store) Close() error { return s.db.Close() }

// PutEnvelope writes env to the envelopes bucket, keyed by env.ID.
func (s *Store) PutEnvelope(env *protocol.ApprovalEnvelope) error {
	if env == nil || env.ID == "" {
		return errors.New("daemon: envelope missing ID")
	}
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(envelopesBucket).Put([]byte(env.ID), data)
	})
}

// GetEnvelope returns the envelope with the given ID.
func (s *Store) GetEnvelope(id string) (*protocol.ApprovalEnvelope, error) {
	var out protocol.ApprovalEnvelope
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(envelopesBucket).Get([]byte(id))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ListEnvelopes returns up to limit envelopes in ascending ID order
// (ULID is monotonic, so this is also chronological). The returned
// slice excludes envelopes that already have a stored answer.
func (s *Store) ListEnvelopes(limit int) ([]*protocol.ApprovalEnvelope, error) {
	if limit <= 0 {
		limit = 50
	}
	var out []*protocol.ApprovalEnvelope
	err := s.db.View(func(tx *bolt.Tx) error {
		answers := tx.Bucket(answersBucket)
		c := tx.Bucket(envelopesBucket).Cursor()
		for k, v := c.First(); k != nil && len(out) < limit; k, v = c.Next() {
			if answers.Get(k) != nil {
				continue
			}
			var e protocol.ApprovalEnvelope
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out = append(out, &e)
		}
		return nil
	})
	return out, err
}

// PutAnswer stores an Answer keyed by EnvelopeID.
func (s *Store) PutAnswer(a *protocol.Answer) error {
	if a == nil || a.EnvelopeID == "" {
		return errors.New("daemon: answer missing envelope_id")
	}
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(answersBucket).Put([]byte(a.EnvelopeID), data)
	})
}

// GetAnswer returns the stored answer for envelopeID, or ErrNotFound.
func (s *Store) GetAnswer(envelopeID string) (*protocol.Answer, error) {
	var out protocol.Answer
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(answersBucket).Get([]byte(envelopeID))
		if raw == nil {
			return ErrNotFound
		}
		return json.Unmarshal(raw, &out)
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
