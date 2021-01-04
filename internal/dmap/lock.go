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
func (dm *DMap) unlockKey(name, key string, token []byte) error {
	lkey := name + key
	// Only one unlockKey should work for a given key.
	dm.service.locker.Lock(lkey)
	defer func() {
		err := dm.service.locker.Unlock(lkey)
		if err != nil {
			dm.service.log.V(3).Printf("[ERROR] Failed to release the fine grained lock for key: %s on DMap: %s: %v", key, name, err)
		}
	}()

	// get the key to check its value
	entry, err := dm.get(name, key)
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
	err = dm.deleteKey(name, key)
	if err != nil {
		return fmt.Errorf("unlock failed because of delete: %w", err)
	}
	return nil
}

// unlock takes key and token and tries to unlock the key.
// It redirects the request to the partition owner, if required.
func (dm *DMap) unlock(name, key string, token []byte) error {
	hkey := partitions.HKey(name, key)
	member := dm.service.primary.PartitionByHKey(hkey).Owner()
	if member.CompareByName(dm.service.rt.This()) {
		return dm.unlockKey(name, key, token)
	}
	req := protocol.NewDMapMessage(protocol.OpUnlock)
	req.SetDMap(name)
	req.SetKey(key)
	req.SetValue(token)
	_, err := dm.service.client.RequestTo2(member.String(), req)
	return err
}

// Unlock releases the lock.
func (l *LockContext) Unlock() error {
	return l.dm.unlock(l.name, l.key, l.token)
}

// tryLock takes a deadline and writeop and sets a key-value pair by using
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
		case <-dm.service.ctx.Done():
			return fmt.Errorf("server is gone")
		}
	}
	return nil
}

// lockKey prepares a token and writeop calls tryLock
func (dm *DMap) lockKey(opcode protocol.OpCode, name, key string,
	timeout, deadline time.Duration) (*LockContext, error) {
	token := make([]byte, 16)
	_, err := rand.Read(token)
	if err != nil {
		return nil, err
	}
	e, err := dm.prepareAndSerialize(opcode, name, key, token, timeout, IfNotFound)
	if err != nil {
		return nil, err
	}
	err = dm.tryLock(e, deadline)
	if err != nil {
		return nil, err
	}
	return &LockContext{
		name:  name,
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
	return dm.lockKey(protocol.OpPutIfEx, dm.name, key, timeout, deadline)
}

// Lock sets a lock for the given key. Acquired lock is only for the key in this dmap.
//
// It returns immediately if it acquires the lock for the given key. Otherwise, it waits until deadline.
//
// You should know that the locks are approximate, and only to be used for non-critical purposes.
func (dm *DMap) Lock(key string, deadline time.Duration) (*LockContext, error) {
	return dm.lockKey(protocol.OpPutIf, dm.name, key, nilTimeout, deadline)
}
