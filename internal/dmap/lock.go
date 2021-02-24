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

package dmap

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/buraksezer/olric/internal/cluster/partitions"
	"github.com/buraksezer/olric/internal/protocol"
)

var (
	// ErrLockNotAcquired is returned when the requested lock could not be acquired
	ErrLockNotAcquired = errors.New("lock not acquired")

	// ErrNoSuchLock is returned when the requested lock does not exist
	ErrNoSuchLock = errors.New("no such lock")
)

// LockContext is returned by Lock and LockWithTimeout methods.
// It should be stored in a proper way to release the lock.
type LockContext struct {
	name  string
	key   string
	token []byte
	dm    *DMap
}

// unlockKey tries to unlock the lock by verifying the lock with token.
func (dm *DMap) unlockKey(key string, token []byte) error {
	lkey := dm.name + key
	// Only one unlockKey should work for a given key.
	dm.s.locker.Lock(lkey)
	defer func() {
		err := dm.s.locker.Unlock(lkey)
		if err != nil {
			dm.s.log.V(3).Printf("[ERROR] Failed to release the fine grained lock for key: %s on DMap: %s: %v", key, dm.name, err)
		}
	}()

	// get the key to check its value
	entry, err := dm.get(key)
	if err == ErrKeyNotFound {
		return ErrNoSuchLock
	}
	if err != nil {
		return err
	}
	val, err := dm.unmarshalValue(entry.Value())
	if err != nil {
		return err
	}

	// the locks is released by the node(timeout) or the user
	if !bytes.Equal(val.([]byte), token) {
		return ErrNoSuchLock
	}

	// release it.
	err = dm.deleteKey(key)
	if err != nil {
		return fmt.Errorf("unlock failed because of delete: %w", err)
	}
	return nil
}

// unlock takes key and token and tries to unlock the key.
// It redirects the request to the partition owner, if required.
func (dm *DMap) unlock(key string, token []byte) error {
	hkey := partitions.HKey(dm.name, key)
	member := dm.s.primary.PartitionByHKey(hkey).Owner()
	if member.CompareByName(dm.s.rt.This()) {
		return dm.unlockKey(key, token)
	}
	req := protocol.NewDMapMessage(protocol.OpUnlock)
	req.SetDMap(dm.name)
	req.SetKey(key)
	req.SetValue(token)
	_, err := dm.s.client.RequestTo2(member.String(), req)
	return err
}

// Unlock releases the lock.
func (l *LockContext) Unlock() error {
	return l.dm.unlock(l.key, l.token)
}

// tryLock takes a deadline and env and sets a key-value pair by using
// PutIf or PutIfEx commands. It tries to acquire the lock 100 times per second
// if the lock is already acquired. It returns ErrLockNotAcquired if the deadline exceeds.
func (dm *DMap) tryLock(e *env, deadline time.Duration) error {
	err := dm.put(e)
	if err == nil {
		return nil
	}
	// If it returns ErrKeyFound, the lock is already acquired.
	if err != ErrKeyFound {
		// something went wrong
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()

	// Try to acquire lock.
LOOP:
	for {
		timer.Reset(10 * time.Millisecond)
		select {
		case <-timer.C:
			err = dm.put(e)
			if err == ErrKeyFound {
				// not released by the other process/goroutine. try again.
				continue
			}
			if err != nil {
				// something went wrong.
				return err
			}
			// Acquired! Quit without error.
			break LOOP
		case <-ctx.Done():
			// Deadline exceeded. Quit with an error.
			return ErrLockNotAcquired
		case <-dm.s.ctx.Done():
			return fmt.Errorf("server is gone")
		}
	}
	return nil
}

// lockKey prepares a token and env calls tryLock
func (dm *DMap) lockKey(opcode protocol.OpCode, key string, timeout, deadline time.Duration) (*LockContext, error) {
	token := make([]byte, 16)
	_, err := rand.Read(token)
	if err != nil {
		return nil, err
	}
	e, err := dm.prepareAndSerialize(opcode, key, token, timeout, IfNotFound)
	if err != nil {
		return nil, err
	}
	err = dm.tryLock(e, deadline)
	if err != nil {
		return nil, err
	}
	return &LockContext{
		name:  dm.name, // TODO: Useless
		key:   key,
		token: token,
		dm:    dm,
	}, nil
}

// LockWithTimeout sets a lock for the given key. If the lock is still unreleased the end of given period of time,
// it automatically releases the lock. Acquired lock is only for the key in this dmap.
//
// It returns immediately if it acquires the lock for the given key. Otherwise, it waits until deadline.
//
// You should know that the locks are approximate, and only to be used for non-critical purposes.
func (dm *DMap) LockWithTimeout(key string, timeout, deadline time.Duration) (*LockContext, error) {
	return dm.lockKey(protocol.OpPutIfEx, key, timeout, deadline)
}

// Lock sets a lock for the given key. Acquired lock is only for the key in this dmap.
//
// It returns immediately if it acquires the lock for the given key. Otherwise, it waits until deadline.
//
// You should know that the locks are approximate, and only to be used for non-critical purposes.
func (dm *DMap) Lock(key string, deadline time.Duration) (*LockContext, error) {
	return dm.lockKey(protocol.OpPutIf, key, nilTimeout, deadline)
}
