package memrepo

import (
	"context"

	"github.com/GoldFridge/factorflow/internal/platform/apperr"
	"github.com/GoldFridge/factorflow/internal/platform/objects"
	"github.com/GoldFridge/factorflow/internal/platform/postgres"
)

// Objects returns the in-memory object store.
func (s *Store) Objects() objects.Store { return (*objectStore)(s) }

// Object reads stored ciphertext back.
func (s *Store) Object(key string) (objects.Object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.blobs[key]
	return stored, ok
}

type objectStore Store

func (o *objectStore) store() *Store { return (*Store)(o) }

// Put stores ciphertext, and keeps what is already there.
//
// The database inserts on conflict do nothing, for a reason worth mirroring: the key is
// derived from the bytes, so a second put of the same key is the same document arriving
// twice, and the first copy is as good as the second.
func (o *objectStore) Put(_ context.Context, _ postgres.Querier, object *objects.Object) error {
	s := o.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.blobs[object.Key]; exists {
		return nil
	}
	s.blobs[object.Key] = *object
	return nil
}

func (o *objectStore) Get(_ context.Context, _ postgres.Querier, key string) (*objects.Object, error) {
	s := o.store()
	s.mu.Lock()
	defer s.mu.Unlock()

	stored, ok := s.blobs[key]
	if !ok {
		return nil, apperr.NotFoundf("object %s", key)
	}
	copied := stored
	copied.Ciphertext = append([]byte(nil), stored.Ciphertext...)
	return &copied, nil
}
