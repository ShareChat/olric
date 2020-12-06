// Copyright 2018-2020 Burak Sezer
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*Package kvstore implements a GC friendly in-memory storage engine by using map and byte array. It also supports compaction.*/
package kvstore

import (
	"regexp"

	"github.com/buraksezer/olric/internal/storage"
	"github.com/vmihailenco/msgpack"
)

const (
	maxGarbageRatio = 0.40
	// 65kb
	minimumSize = 1 << 16
)

// KVStore implements a new off-heap data store which uses built-in map to
// keep metadata and mmap syscall for allocating memory to store values.
// The allocated memory is not a subject of Golang's GC.
type KVStore struct {
	tables  []*table
	options *storage.Options
}

func DefaultOptions() *storage.Options {
	options := storage.NewOptions()
	options.Add("TableSize", minimumSize)
	return options
}

// New creates a new KVStore instance.
func New(options *storage.Options) (*KVStore, error) {
	size, err := options.Get("TableSize")
	if err != nil {
		return nil, err
	}
	kv := &KVStore{
		options: options,
	}
	t := newTable(size.(int))
	kv.tables = append(kv.tables, t)
	return kv, nil
}

func (kv *KVStore) Fork() (storage.Engine, error) {
	return New(kv.options)
}

func (kv *KVStore) Name() string {
	return "io.olric.kvstore"
}

func (kv *KVStore) NewEntry() storage.Entry {
	return NewEntry()
}

// PutRaw sets the raw value for the given key.
func (kv *KVStore) PutRaw(hkey uint64, value []byte) error {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	var res error
	for {
		// Get the last value, storage only calls Put on the last created table.
		t := kv.tables[len(kv.tables)-1]
		err := t.putRaw(hkey, value)
		if err == errNotEnoughSpace {
			// Create a new table and put the new k/v pair in it.
			nt := newTable(kv.Inuse() * 2)
			kv.tables = append(kv.tables, nt)
			res = storage.ErrFragmented
			// try again
			continue
		}
		if err != nil {
			return err
		}
		// everything is ok
		break
	}
	return res
}

// Put sets the value for the given key. It overwrites any previous value for that key
func (kv *KVStore) Put(hkey uint64, value storage.Entry) error {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	var res error
	for {
		// Get the last value, storage only calls Put on the last created table.
		t := kv.tables[len(kv.tables)-1]
		err := t.put(hkey, value)
		if err == errNotEnoughSpace {
			// Create a new table and put the new k/v pair in it.
			nt := newTable(kv.Inuse() * 2)
			kv.tables = append(kv.tables, nt)
			res = storage.ErrFragmented
			// try again
			continue
		}
		if err != nil {
			return err
		}
		// everything is ok
		break
	}
	return res
}

// GetRaw extracts encoded value for the given hkey. This is useful for merging tables.
func (kv *KVStore) GetRaw(hkey uint64) ([]byte, error) {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	// Scan available tables by starting the last added table.
	for i := len(kv.tables) - 1; i >= 0; i-- {
		t := kv.tables[i]
		rawval, prev := t.getRaw(hkey)
		if prev {
			// Try out the other tables.
			continue
		}
		// Found the key, return the stored value with its metadata.
		return rawval, nil
	}

	// Nothing here.
	return nil, storage.ErrKeyNotFound
}

// Get gets the value for the given key. It returns storage.ErrKeyNotFound if the DB
// does not contains the key. The returned Entry is its own copy,
// it is safe to modify the contents of the returned slice.
func (kv *KVStore) Get(hkey uint64) (storage.Entry, error) {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	// Scan available tables by starting the last added table.
	for i := len(kv.tables) - 1; i >= 0; i-- {
		t := kv.tables[i]
		res, prev := t.get(hkey)
		if prev {
			// Try out the other tables.
			continue
		}
		// Found the key, return the stored value with its metadata.
		return res, nil
	}
	// Nothing here.
	return nil, storage.ErrKeyNotFound
}

// GetTTL gets the timeout for the given key. It returns storage.ErrKeyNotFound if the DB
// does not contains the key.
func (kv *KVStore) GetTTL(hkey uint64) (int64, error) {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	// Scan available tables by starting the last added table.
	for i := len(kv.tables) - 1; i >= 0; i-- {
		t := kv.tables[i]
		ttl, prev := t.getTTL(hkey)
		if prev {
			// Try out the other tables.
			continue
		}
		// Found the key, return its ttl
		return ttl, nil
	}
	// Nothing here.
	return 0, storage.ErrKeyNotFound
}

// GetKey gets the key for the given hkey. It returns storage.ErrKeyNotFound if the DB
// does not contains the key.
func (kv *KVStore) GetKey(hkey uint64) (string, error) {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	// Scan available tables by starting the last added table.
	for i := len(kv.tables) - 1; i >= 0; i-- {
		t := kv.tables[i]
		key, prev := t.getKey(hkey)
		if prev {
			// Try out the other tables.
			continue
		}
		// Found the key, return its ttl
		return key, nil
	}
	// Nothing here.
	return "", storage.ErrKeyNotFound
}

// Delete deletes the value for the given key. Delete will not returns error if key doesn't exist.
func (kv *KVStore) Delete(hkey uint64) error {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	// Scan available tables by starting the last added table.
	for i := len(kv.tables) - 1; i >= 0; i-- {
		t := kv.tables[i]
		if prev := t.delete(hkey); prev {
			// Try out the other tables.
			continue
		}
		break
	}

	if len(kv.tables) != 1 {
		return nil
	}

	t := kv.tables[0]
	if float64(t.allocated)*maxGarbageRatio <= float64(t.garbage) {
		// Create a new table here.
		nt := newTable(kv.Inuse() * 2)
		kv.tables = append(kv.tables, nt)
		return storage.ErrFragmented
	}
	return nil
}

// UpdateTTL updates the expiry for the given key.
func (kv *KVStore) UpdateTTL(hkey uint64, data storage.Entry) error {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	// Scan available tables by starting the last added table.
	for i := len(kv.tables) - 1; i >= 0; i-- {
		t := kv.tables[i]
		prev := t.updateTTL(hkey, data)
		if prev {
			// Try out the other tables.
			continue
		}
		// Found the key, return the stored value with its metadata.
		return nil
	}
	// Nothing here.
	return storage.ErrKeyNotFound
}

type transport struct {
	HKeys     map[uint64]int
	Memory    []byte
	Offset    int
	Allocated int
	Inuse     int
	Garbage   int
}

// Export serializes underlying data structes into a byte slice. It may return
// ErrFragmented if the tables are fragmented. If you get this error, you should
// try to call Export again some time later.
func (kv *KVStore) Export() ([]byte, error) {
	if len(kv.tables) != 1 {
		return nil, storage.ErrFragmented
	}
	t := kv.tables[0]
	tr := &transport{
		HKeys:     t.hkeys,
		Offset:    t.offset,
		Allocated: t.allocated,
		Inuse:     t.inuse,
		Garbage:   t.garbage,
	}
	tr.Memory = make([]byte, t.offset+1)
	copy(tr.Memory, t.memory[:t.offset])
	return msgpack.Marshal(tr)
}

// Import gets the serialized data by Export and creates a new storage instance.
func (kv *KVStore) Import(data []byte) (storage.Engine, error) {
	tr := transport{}
	err := msgpack.Unmarshal(data, &tr)
	if err != nil {
		return nil, err
	}

	options := kv.options.Copy()
	options.Add("TableSize", tr.Allocated)
	fresh, err := New(options)
	if err != nil {
		return nil, err
	}
	t := fresh.tables[0]
	t.hkeys = tr.HKeys
	t.offset = tr.Offset
	t.inuse = tr.Inuse
	t.garbage = tr.Garbage
	copy(t.memory, tr.Memory)
	return fresh, nil
}

// Len returns the key cound in this storage.
func (kv *KVStore) Len() int {
	var total int
	for _, t := range kv.tables {
		total += len(t.hkeys)
	}
	return total
}

// Stats is a function which provides memory allocation and garbage ratio of a storage instance.
func (kv *KVStore) Stats() map[string]int {
	stats := map[string]int{
		"allocated": 0,
		"inuse":     0,
		"garbage":   0,
	}
	for _, t := range kv.tables {
		stats["allocated"] += t.allocated
		stats["inuse"] += t.inuse
		stats["garbage"] += t.garbage

	}
	return stats
}

// NumTables returns the number of tables in a storage instance.
func (kv *KVStore) NumTables() int {
	return len(kv.tables)
}

// Inuse returns total in-use space by the tables.
func (kv *KVStore) Inuse() int {
	// Stats does the same thing but we need
	// to eliminate useless calls.
	inuse := 0
	for _, t := range kv.tables {
		inuse += t.inuse
	}
	return inuse
}

// Check checks the key existence.
func (kv *KVStore) Check(hkey uint64) bool {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	// Scan available tables by starting the last added table.
	for i := len(kv.tables) - 1; i >= 0; i-- {
		t := kv.tables[i]
		_, ok := t.hkeys[hkey]
		if ok {
			return true
		}
	}
	// Nothing there.
	return false
}

// Range calls f sequentially for each key and value present in the map.
// If f returns false, range stops the iteration. Range may be O(N) with
// the number of elements in the map even if f returns false after a constant
// number of calls.
func (kv *KVStore) Range(f func(hkey uint64, entry storage.Entry) bool) {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}

	// Scan available tables by starting the last added table.
	for i := len(kv.tables) - 1; i >= 0; i-- {
		t := kv.tables[i]
		for hkey := range t.hkeys {
			entry, _ := t.get(hkey)
			if !f(hkey, entry) {
				break
			}
		}
	}
}

// MatchOnKey calls a regular expression on keys and provides an iterator.
func (kv *KVStore) MatchOnKey(expr string, f func(hkey uint64, entry storage.Entry) bool) error {
	if len(kv.tables) == 0 {
		panic("tables cannot be empty")
	}
	r, err := regexp.Compile(expr)
	if err != nil {
		return err
	}

	// Scan available tables by starting the last added table.
	for i := len(kv.tables) - 1; i >= 0; i-- {
		t := kv.tables[i]
		for hkey := range t.hkeys {
			key, _ := t.getRawKey(hkey)
			if !r.Match(key) {
				continue
			}
			data, _ := t.get(hkey)
			if !f(hkey, data) {
				return nil
			}
		}
	}
	return nil
}

func (kv *KVStore) Close() error {
	return nil
}
