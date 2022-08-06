/*
   Copyright 2022 Erigon contributors
   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at
       http://www.apache.org/licenses/LICENSE-2.0
   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package memdb

import (
	"bytes"
	"fmt"

	"github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
)

type NextType int

const (
	Normal NextType = iota
	Dup
	NoDup
)

// entry for the cursor
type cursorEntry struct {
	key   []byte
	value []byte
}

// cursor
type memoryMutationCursor struct {
	cursor    kv.CursorDupSort
	memCursor kv.RwCursorDupSort

	isPrevFromDb bool
	// entry history TODO(yperbasis): remove
	currentDbEntry  cursorEntry
	currentMemEntry cursorEntry
	// we keep the mining mutation so that we can insert new elements in db
	mutation *MemoryMutation
	table    string
}

// First move cursor to first position and return key and value accordingly.
func (m *memoryMutationCursor) First() ([]byte, []byte, error) {
	memKey, memValue, err := m.memCursor.First()
	if err != nil {
		return nil, nil, err
	}

	if m.mutation.isTableCleared(m.table) {
		m.isPrevFromDb = false
		return memKey, memValue, err
	}

	dbKey, dbValue, err := m.cursor.First()
	if err != nil {
		return nil, nil, err
	}

	if dbKey != nil && m.mutation.isEntryDeleted(m.table, dbKey) {
		if dbKey, dbValue, err = m.getNextOnDb(Normal); err != nil {
			return nil, nil, err
		}
	}

	return m.goForward(memKey, memValue, dbKey, dbValue, Normal)
}

func (m *memoryMutationCursor) getNextOnDb(t NextType) (key []byte, value []byte, err error) {
	switch t {
	case Normal:
		key, value, err = m.cursor.Next()
		if err != nil {
			return
		}
	case Dup:
		key, value, err = m.cursor.NextDup()
		if err != nil {
			return
		}
	case NoDup:
		key, value, err = m.cursor.NextNoDup()
		if err != nil {
			return
		}
	default:
		err = fmt.Errorf("invalid next type")
		return
	}

	for key != nil && value != nil && m.mutation.isEntryDeleted(m.table, m.convertAutoDupsort(key, value)) {
		switch t {
		case Normal:
			key, value, err = m.cursor.Next()
			if err != nil {
				return
			}
		case Dup:
			key, value, err = m.cursor.NextDup()
			if err != nil {
				return
			}
		case NoDup:
			key, value, err = m.cursor.NextNoDup()
			if err != nil {
				return
			}
		default:
			err = fmt.Errorf("invalid next type")
			return
		}
	}
	return
}

func (m *memoryMutationCursor) convertAutoDupsort(key []byte, value []byte) []byte {
	config, ok := kv.ChaindataTablesCfg[m.table]
	// If we do not have the configuration we assume it is not dupsorted
	if !ok || !config.AutoDupSortKeysConversion {
		return key
	}
	if len(key) != config.DupToLen {
		return key
	}
	return append(key, value[:config.DupFromLen-config.DupToLen]...)
}

// Current return the current key and values the cursor is on.
func (m *memoryMutationCursor) Current() ([]byte, []byte, error) {
	if m.isPrevFromDb {
		return m.cursor.Current()
	} else {
		return m.memCursor.Current()
	}
}

func (m *memoryMutationCursor) skipIntersection(memKey, memValue, dbKey, dbValue []byte, t NextType) (newDbKey []byte, newDbValue []byte, err error) {
	newDbKey = dbKey
	newDbValue = dbValue
	config, ok := kv.ChaindataTablesCfg[m.table]
	dupsortOffset := 0
	if ok && config.AutoDupSortKeysConversion {
		dupsortOffset = config.DupFromLen - config.DupToLen
	}
	// Check for duplicates
	if bytes.Equal(memKey, dbKey) {
		if (config.Flags & kv.DupSort) == 0 {
			if newDbKey, newDbValue, err = m.getNextOnDb(t); err != nil {
				return
			}
		} else if bytes.Equal(memValue, dbValue) {
			if newDbKey, newDbValue, err = m.getNextOnDb(t); err != nil {
				return
			}
		} else if dupsortOffset != 0 && len(memValue) >= dupsortOffset && len(dbValue) >= dupsortOffset && bytes.Equal(memValue[:dupsortOffset], dbValue[:dupsortOffset]) {
			if newDbKey, newDbValue, err = m.getNextOnDb(t); err != nil {
				return
			}
		}
	}
	return
}

func (m *memoryMutationCursor) goForward(memKey, memValue, dbKey, dbValue []byte, t NextType) ([]byte, []byte, error) {
	var err error
	if memValue == nil && dbValue == nil {
		return nil, nil, nil
	}

	dbKey, dbValue, err = m.skipIntersection(memKey, memValue, dbKey, dbValue, t)
	if err != nil {
		return nil, nil, err
	}

	m.currentDbEntry = cursorEntry{dbKey, dbValue}
	m.currentMemEntry = cursorEntry{memKey, memValue}
	// compare entries
	if bytes.Equal(memKey, dbKey) {
		m.isPrevFromDb = dbValue != nil && (memValue == nil || bytes.Compare(memValue, dbValue) > 0)
	} else {
		m.isPrevFromDb = dbValue != nil && (memKey == nil || bytes.Compare(memKey, dbKey) > 0)
	}
	if dbValue == nil {
		m.currentDbEntry = cursorEntry{}
	}
	if memValue == nil {
		m.currentMemEntry = cursorEntry{}
	}
	if m.isPrevFromDb {
		return dbKey, dbValue, nil
	}

	return memKey, memValue, nil
}

// Next returns the next element of the mutation.
func (m *memoryMutationCursor) Next() ([]byte, []byte, error) {
	if m.isPrevFromDb {
		k, v, err := m.getNextOnDb(Normal)
		if err != nil {
			return nil, nil, err
		}
		return m.goForward(m.currentMemEntry.key, m.currentMemEntry.value, k, v, Normal)
	}

	memK, memV, err := m.memCursor.Next()
	if err != nil {
		return nil, nil, err
	}

	return m.goForward(memK, memV, m.currentDbEntry.key, m.currentDbEntry.value, Normal)
}

// NextDup returns the next element of the mutation.
func (m *memoryMutationCursor) NextDup() ([]byte, []byte, error) {
	if m.isPrevFromDb {
		k, v, err := m.getNextOnDb(Dup)

		if err != nil {
			return nil, nil, err
		}
		return m.goForward(m.currentMemEntry.key, m.currentMemEntry.value, k, v, Dup)
	}

	memK, memV, err := m.memCursor.NextDup()
	if err != nil {
		return nil, nil, err
	}

	return m.goForward(memK, memV, m.currentDbEntry.key, m.currentDbEntry.value, Dup)
}

// Seek move pointer to a key at a certain position.
func (m *memoryMutationCursor) Seek(seek []byte) ([]byte, []byte, error) {
	if m.mutation.isTableCleared(m.table) {
		return m.memCursor.Seek(seek)
	}

	dbKey, dbValue, err := m.cursor.Seek(seek)
	if err != nil {
		return nil, nil, err
	}

	// If the entry is marked as DB find one that is not
	if dbKey != nil && m.mutation.isEntryDeleted(m.table, dbKey) {
		dbKey, dbValue, err = m.getNextOnDb(Normal)
		if err != nil {
			return nil, nil, err
		}
	}

	memKey, memValue, err := m.memCursor.Seek(seek)
	if err != nil {
		return nil, nil, err
	}

	return m.goForward(memKey, memValue, dbKey, dbValue, Normal)
}

// Seek move pointer to a key at a certain position.
func (m *memoryMutationCursor) SeekExact(seek []byte) ([]byte, []byte, error) {
	memKey, memValue, err := m.memCursor.SeekExact(seek)
	if err != nil {
		return nil, nil, err
	}

	if memKey != nil {
		m.currentMemEntry.key = memKey
		m.currentMemEntry.value = memValue
		m.currentDbEntry.key, m.currentDbEntry.value, err = m.cursor.Seek(seek)
		m.isPrevFromDb = false
		return memKey, memValue, err
	}

	dbKey, dbValue, err := m.cursor.SeekExact(seek)
	if err != nil {
		return nil, nil, err
	}

	if dbKey != nil && !m.mutation.isTableCleared(m.table) && !m.mutation.isEntryDeleted(m.table, seek) {
		m.currentDbEntry.key = dbKey
		m.currentDbEntry.value = dbValue
		m.currentMemEntry.key, m.currentMemEntry.value, err = m.memCursor.Seek(seek)
		m.isPrevFromDb = true
		return dbKey, dbValue, err
	}
	return nil, nil, nil
}

func (m *memoryMutationCursor) Put(k, v []byte) error {
	return m.mutation.Put(m.table, common.Copy(k), common.Copy(v))
}

func (m *memoryMutationCursor) Append(k []byte, v []byte) error {
	return m.mutation.Append(m.table, common.Copy(k), common.Copy(v))

}

func (m *memoryMutationCursor) AppendDup(k []byte, v []byte) error {
	return m.memCursor.AppendDup(common.Copy(k), common.Copy(v))
}

func (m *memoryMutationCursor) PutNoDupData(key, value []byte) error {
	panic("DeleteCurrentDuplicates Not implemented")
}

func (m *memoryMutationCursor) Delete(k []byte) error {
	return m.mutation.Delete(m.table, k)
}

func (m *memoryMutationCursor) DeleteCurrent() error {
	panic("DeleteCurrent Not implemented")
}
func (m *memoryMutationCursor) DeleteExact(k1, k2 []byte) error {
	panic("DeleteExact Not implemented")
}

// TODO(yperbasis): FIXME
func (m *memoryMutationCursor) DeleteCurrentDuplicates() error {
	k, _, err := m.cursor.Current()
	if err != nil {
		return err
	}
	for v, err := m.cursor.SeekBothRange(k, nil); v != nil; k, v, err = m.cursor.NextDup() {
		if err != nil {
			return err
		}
		if err := m.Delete(k); err != nil {
			return err
		}
	}
	dbK, _, err := m.memCursor.Current()
	if err != nil {
		return err
	}
	if len(dbK) > 0 {
		return m.memCursor.DeleteCurrentDuplicates()
	}
	return nil
}

// Seek move pointer to a key at a certain position.
func (m *memoryMutationCursor) SeekBothRange(key, value []byte) ([]byte, error) {
	if value == nil {
		_, v, err := m.SeekExact(key)
		return v, err
	}

	dbValue, err := m.cursor.SeekBothRange(key, value)
	if err != nil {
		return nil, err
	}

	if dbValue != nil && m.mutation.isEntryDeleted(m.table, m.convertAutoDupsort(key, dbValue)) {
		_, dbValue, err = m.getNextOnDb(Dup)
		if err != nil {
			return nil, err
		}
	}

	memValue, err := m.memCursor.SeekBothRange(key, value)
	if err != nil {
		return nil, err
	}
	_, retValue, err := m.goForward(key, memValue, key, dbValue, Normal)
	return retValue, err
}

func (m *memoryMutationCursor) Last() ([]byte, []byte, error) {
	memKey, memValue, err := m.memCursor.Last()
	if err != nil {
		return nil, nil, err
	}
	dbKey, dbValue, err := m.cursor.Last()
	if err != nil {
		return nil, nil, err
	}

	dbKey, dbValue, err = m.skipIntersection(memKey, memValue, dbKey, dbValue, Normal)
	if err != nil {
		return nil, nil, err
	}

	m.currentDbEntry = cursorEntry{dbKey, dbValue}
	m.currentMemEntry = cursorEntry{memKey, memValue}

	// Basic checks
	if dbKey != nil && m.mutation.isEntryDeleted(m.table, dbKey) {
		m.currentDbEntry = cursorEntry{}
		m.isPrevFromDb = false
		return memKey, memValue, nil
	}

	if dbValue == nil {
		m.isPrevFromDb = false
		return memKey, memValue, nil
	}

	if memValue == nil {
		m.isPrevFromDb = true
		return dbKey, dbValue, nil
	}
	// Check which one is last and return it
	keyCompare := bytes.Compare(memKey, dbKey)
	if keyCompare == 0 {
		if bytes.Compare(memValue, dbValue) > 0 {
			m.currentDbEntry = cursorEntry{}
			m.isPrevFromDb = false
			return memKey, memValue, nil
		}
		m.currentMemEntry = cursorEntry{}
		m.isPrevFromDb = true
		return dbKey, dbValue, nil
	}

	if keyCompare > 0 {
		m.currentDbEntry = cursorEntry{}
		m.isPrevFromDb = false
		return memKey, memValue, nil
	}

	m.currentMemEntry = cursorEntry{}
	m.isPrevFromDb = true
	return dbKey, dbValue, nil
}

func (m *memoryMutationCursor) Prev() ([]byte, []byte, error) {
	panic("Prev is not implemented!")
}

func (m *memoryMutationCursor) Close() {
	if m.cursor != nil {
		m.cursor.Close()
	}
	if m.memCursor != nil {
		m.memCursor.Close()
	}
}

func (m *memoryMutationCursor) Count() (uint64, error) {
	panic("Not implemented")
}

func (m *memoryMutationCursor) FirstDup() ([]byte, error) {
	panic("Not implemented")
}

func (m *memoryMutationCursor) NextNoDup() ([]byte, []byte, error) {
	if m.isPrevFromDb {
		k, v, err := m.getNextOnDb(NoDup)
		if err != nil {
			return nil, nil, err
		}
		return m.goForward(m.currentMemEntry.key, m.currentMemEntry.value, k, v, NoDup)
	}

	memK, memV, err := m.memCursor.NextNoDup()
	if err != nil {
		return nil, nil, err
	}

	return m.goForward(memK, memV, m.currentDbEntry.key, m.currentDbEntry.value, NoDup)
}

func (m *memoryMutationCursor) LastDup() ([]byte, error) {
	panic("Not implemented")
}

func (m *memoryMutationCursor) CountDuplicates() (uint64, error) {
	panic("Not implemented")
}

func (m *memoryMutationCursor) SeekBothExact(key, value []byte) ([]byte, []byte, error) {
	panic("SeekBothExact Not implemented")
}
