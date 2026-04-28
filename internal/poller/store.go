package poller

import (
	"encoding/binary"
	"fmt"

	"go.etcd.io/bbolt"
)

var (
	bucketCursors = []byte("event_cursors")
	bucketMeta    = []byte("meta")
)

// Store tracks the highest event timestamp we've already emitted for a
// given (app, machine). Events with timestamps <= the cursor are
// considered already-seen on poller restart.
type Store struct {
	db *bbolt.DB
}

func OpenStore(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("open boltdb: %w", err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(bucketCursors); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(bucketMeta)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func cursorKey(app, machineID string) []byte {
	return []byte(app + "/" + machineID)
}

// LastSeen returns the highest event timestamp (unix ms) we've emitted
// for the given machine, or 0 if none.
func (s *Store) LastSeen(app, machineID string) (int64, error) {
	var ts int64
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketCursors)
		v := b.Get(cursorKey(app, machineID))
		if len(v) == 8 {
			ts = int64(binary.BigEndian.Uint64(v))
		}
		return nil
	})
	return ts, err
}

func (s *Store) SetLastSeen(app, machineID string, ts int64) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketCursors)
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, uint64(ts))
		return b.Put(cursorKey(app, machineID), buf)
	})
}

func metaKey(app, name string) []byte {
	return []byte(app + "/" + name)
}

func (s *Store) GetMeta(app, name string) (string, error) {
	var v string
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		raw := b.Get(metaKey(app, name))
		if raw != nil {
			v = string(raw)
		}
		return nil
	})
	return v, err
}

func (s *Store) SetMeta(app, name, value string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		return b.Put(metaKey(app, name), []byte(value))
	})
}
