// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package leveldb

import (
	"awesomeProject1/goleveldb/leveldb/errors"
	"awesomeProject1/goleveldb/leveldb/memdb"
	"awesomeProject1/goleveldb/leveldb/storage"
	"encoding/binary"
	"fmt"
	"io"
	//"log"
)

// ErrBatchCorrupted records reason of batch corruption. This error will be
// wrapped with errors.ErrCorrupted.
type ErrBatchCorrupted struct {
	Reason string
}

func (e *ErrBatchCorrupted) Error() string {
	return fmt.Sprintf("leveldb: batch corrupted: %s", e.Reason)
}

func newErrBatchCorrupted(reason string) error {
	return errors.NewErrCorrupted(storage.FileDesc{}, &ErrBatchCorrupted{reason})
}

const (
	batchHeaderLen = 8 + 4
	batchGrowRec   = 3000
	batchBufioSize = 16
)

// BatchReplay wraps basic batch operations.batch作为数据库操作的最小执行单元
type BatchReplay interface {
	Put(key, value []byte)
	Delete(key []byte)
}

type batchIndex struct {
	keyType            keyType //插入还是删除
	keyPos, keyLen     int     //K长度和内容
	valuePos, valueLen int     //V长度和内容
}

func (index batchIndex) k(data []byte) []byte {
	return data[index.keyPos : index.keyPos+index.keyLen]
}

func (index batchIndex) v(data []byte) []byte {
	if index.valueLen != 0 {
		return data[index.valuePos : index.valuePos+index.valueLen]
	}
	return nil
}

func (index batchIndex) kv(data []byte) (key, value []byte) {
	return index.k(data), index.v(data)
}

// Batch is a write batch.
type Batch struct {
	data  []byte       //存kv对
	index []batchIndex //内部的kv索引

	// internalLen is sums of key/value pair length plus 8-bytes internal key. key+8个字节，作为internalKey，这里是key的长度
	//这八个字节用于存储该操作对应的sequence number计时器7bytes，累加，数值越大表明数据越新、该操作的类型1byte
	internalLen int //batch的大小
}

// 继承：Batch_s is a Batch
type Batch_s struct {
	Batch
}

func (b *Batch) grow(n int) {
	o := len(b.data)
	if cap(b.data)-o < n {
		div := 1
		if len(b.index) > batchGrowRec {
			div = len(b.index) / batchGrowRec
		}
		ndata := make([]byte, o, o+n+o/div)
		copy(ndata, b.data)
		b.data = ndata
	}
}

func (b *Batch) appendRec(kt keyType, key, value []byte) {
	n := 1 + binary.MaxVarintLen32 + len(key)
	if kt == keyTypeVal {
		n += binary.MaxVarintLen32 + len(value)
	}
	b.grow(n)
	index := batchIndex{keyType: kt}
	o := len(b.data)
	data := b.data[:o+n]
	data[o] = byte(kt)                                 //kt==1, data[0]=1
	o++                                                // o++ that is 1
	o += binary.PutUvarint(data[o:], uint64(len(key))) //data[1]=len(key) and o=2
	index.keyPos = o
	index.keyLen = len(key)
	o += copy(data[o:], key)
	if kt == keyTypeVal {
		o += binary.PutUvarint(data[o:], uint64(len(value)))
		index.valuePos = o
		index.valueLen = len(value)
		o += copy(data[o:], value) //data的组成 keytype + len(key) + key +  len(value) + value
	}
	b.data = data[:o]
	b.index = append(b.index, index)
	b.internalLen += index.keyLen + index.valueLen + 8
}

// Put appends 'put operation' of the given key/value pair to the batch.
// It is safe to modify the contents of the argument after Put returns but not
// before.
func (b *Batch) Put(key, value []byte) {
	b.appendRec(keyTypeVal, key, value)
}

// Delete appends 'delete operation' of the given key to the batch.
// It is safe to modify the contents of the argument after Delete returns but
// not before.
func (b *Batch) Delete(key []byte) {
	b.appendRec(keyTypeDel, key, nil)
}

// Dump dumps batch contents. The returned slice can be loaded into the
// batch using Load method.
// The returned slice is not its own copy, so the contents should not be
// modified.
func (b *Batch) Dump() []byte {
	return b.data
}

// Load loads given slice into the batch. Previous contents of the batch
// will be discarded.
// The given slice will not be copied and will be used as batch buffer, so
// it is not safe to modify the contents of the slice.
func (b *Batch) Load(data []byte) error {
	return b.decode(data, -1)
}

// Replay replays batch contents.
func (b *Batch) Replay(r BatchReplay) error {
	for _, index := range b.index {
		switch index.keyType {
		case keyTypeVal:
			r.Put(index.k(b.data), index.v(b.data))
		case keyTypeDel:
			r.Delete(index.k(b.data))
		}
	}
	return nil
}

// Len returns number of records in the batch.
func (b *Batch) Len() int {
	return len(b.index)
}

// Reset resets the batch.L
// batch有 data & index & internalLen
func (b *Batch) Reset() {
	b.data = b.data[:0]
	b.index = b.index[:0]
	b.internalLen = 0
}

func (b *Batch) replayInternal(fn func(i int, kt keyType, k, v []byte) error) error {
	for i, index := range b.index {
		if err := fn(i, index.keyType, index.k(b.data), index.v(b.data)); err != nil {
			return err
		}
	}
	return nil
}

func (b *Batch) append(p *Batch) {
	ob := len(b.data)
	oi := len(b.index)
	b.data = append(b.data, p.data...)
	b.index = append(b.index, p.index...)
	b.internalLen += p.internalLen

	// Updating index offset.
	if ob != 0 {
		for ; oi < len(b.index); oi++ {
			index := &b.index[oi]
			index.keyPos += ob
			if index.valueLen != 0 {
				index.valuePos += ob
			}
		}
	}
}

func (b *Batch) decode(data []byte, expectedLen int) error {
	b.data = data
	b.index = b.index[:0]
	b.internalLen = 0
	err := decodeBatch(data, func(i int, index batchIndex) error {
		b.index = append(b.index, index)
		b.internalLen += index.keyLen + index.valueLen + 8
		return nil
	})
	if err != nil {
		return err
	}
	if expectedLen >= 0 && len(b.index) != expectedLen {
		return newErrBatchCorrupted(fmt.Sprintf("invalid records length: %d vs %d", expectedLen, len(b.index)))
	}
	return nil
}

func (b *Batch) putMem(seq uint64, mdb *memdb.DB) error {
	var ik []byte
	for i, index := range b.index {
		ik = makeInternalKey(ik, index.k(b.data), seq+uint64(i), index.keyType)
		//mdb *memdb.DB调用memdb中定义的public Put方法
		//log.Println(ik,index.k(b.data),index.v(b.data))
		//NEW = index.k(b.data)[1:5]
		if err := mdb.Put(ik, index.v(b.data)); err != nil {
			return err
		}
	}
	return nil
}

// 由batch写入mem & revertmem重写的方法
func (b *Batch) putMem_s(seq uint64, mdb *memdb.DBs) error {
	var ik []byte
	//fmt.Println("开始遍历batch，给每个数据加上8bytes的后缀")
	for i, index := range b.index {
		ik = makeInternalKey(ik, index.k(b.data), seq+uint64(i), index.keyType)
		//mdb *memdb.DB调用memdb中定义的public Put方法
		//fmt.Println("每Add一个后缀，就Put进mem中")
		if err := mdb.Put_s(ik, index.v(b.data)); err != nil {
			return err
		}
	}
	//fmt.Println("所有的数据都被Put进入mem,准备返回")
	return nil
}
func (b *Batch) revertMem_s(seq uint64, mdb *memdb.DBs) error {
	var ik []byte
	for i, index := range b.index {
		ik = makeInternalKey(ik, index.k(b.data), seq+uint64(i), index.keyType)
		if err := mdb.Delete_s(ik); err != nil {
			return err
		}
	}
	return nil
}
func (b *Batch) revertMem(seq uint64, mdb *memdb.DB) error {
	var ik []byte
	for i, index := range b.index {
		ik = makeInternalKey(ik, index.k(b.data), seq+uint64(i), index.keyType)
		if err := mdb.Delete(ik); err != nil {
			return err
		}
	}
	return nil
}

func newBatch() interface{} {
	return &Batch{}
}

// MakeBatch returns empty batch with preallocated buffer.
func MakeBatch(n int) *Batch {
	return &Batch{data: make([]byte, 0, n)}
}

func decodeBatch(data []byte, fn func(i int, index batchIndex) error) error {
	var index batchIndex
	for i, o := 0, 0; o < len(data); i++ {
		// Key type.
		index.keyType = keyType(data[o])
		if index.keyType > keyTypeVal {
			return newErrBatchCorrupted(fmt.Sprintf("bad record: invalid type %#x", uint(index.keyType)))
		}
		o++

		// Key.
		x, n := binary.Uvarint(data[o:])
		o += n
		if n <= 0 || o+int(x) > len(data) {
			return newErrBatchCorrupted("bad record: invalid key length")
		}
		index.keyPos = o
		index.keyLen = int(x)
		o += index.keyLen

		// Value.
		if index.keyType == keyTypeVal {
			x, n = binary.Uvarint(data[o:])
			o += n
			if n <= 0 || o+int(x) > len(data) {
				return newErrBatchCorrupted("bad record: invalid value length")
			}
			index.valuePos = o
			index.valueLen = int(x)
			o += index.valueLen
		} else {
			index.valuePos = 0
			index.valueLen = 0
		}

		if err := fn(i, index); err != nil {
			return err
		}
	}
	return nil
}

func decodeBatchToMem(data []byte, expectSeq uint64, mdb *memdb.DB) (seq uint64, batchLen int, err error) {
	seq, batchLen, err = decodeBatchHeader(data)
	if err != nil {
		fmt.Println("err != nil")
		return 0, 0, err
	}
	/*if seq < expectSeq {
		fmt.Println("seq < expectSeq")
		return 0, 0, newErrBatchCorrupted("invalid sequence number")
	}*/
	data = data[batchHeaderLen:]
	var ik []byte
	var decodedLen int
	err = decodeBatch(data, func(i int, index batchIndex) error {
		if i >= batchLen {
			return newErrBatchCorrupted("invalid records length")
		}
		ik = makeInternalKey(ik, index.k(data), seq+uint64(i), index.keyType)
		if err := mdb.Put(ik, index.v(data)); err != nil {
			return err
		}
		decodedLen++
		return nil
	})
	if err == nil && decodedLen != batchLen {
		err = newErrBatchCorrupted(fmt.Sprintf("invalid records length: %d vs %d", batchLen, decodedLen))
	}
	return
}
func decodeBatchToMem_s(data []byte, expectSeq uint64, mdb *memdb.DBs) (seq uint64, batchLen int, err error) {
	seq, batchLen, err = decodeBatchHeader(data)
	if err != nil {
		return 0, 0, err
	}
	/*if seq < expectSeq {
		return 0, 0, newErrBatchCorrupted("invalid sequence number")
	}*/
	data = data[batchHeaderLen:]
	var ik []byte
	var decodedLen int
	err = decodeBatch(data, func(i int, index batchIndex) error {
		if i >= batchLen {
			return newErrBatchCorrupted("invalid records length")
		}
		ik = makeInternalKey(ik, index.k(data), seq+uint64(i), index.keyType)
		if err := mdb.Put_s(ik, index.v(data)); err != nil {
			return err
		}
		decodedLen++
		return nil
	})
	if err == nil && decodedLen != batchLen {
		err = newErrBatchCorrupted(fmt.Sprintf("invalid records length: %d vs %d", batchLen, decodedLen))
	}
	return
}
func encodeBatchHeader(dst []byte, seq uint64, batchLen int) []byte {
	dst = ensureBuffer(dst, batchHeaderLen)
	binary.LittleEndian.PutUint64(dst, seq)
	binary.LittleEndian.PutUint32(dst[8:], uint32(batchLen))
	return dst
}

func decodeBatchHeader(data []byte) (seq uint64, batchLen int, err error) {
	if len(data) < batchHeaderLen {
		return 0, 0, newErrBatchCorrupted("too short")
	}

	seq = binary.LittleEndian.Uint64(data)
	batchLen = int(binary.LittleEndian.Uint32(data[8:]))
	if batchLen < 0 {
		return 0, 0, newErrBatchCorrupted("invalid records length")
	}
	return
}

func batchesLen(batches []*Batch) int {
	batchLen := 0
	for _, batch := range batches {
		batchLen += batch.Len()
	}
	return batchLen
}

func writeBatchesWithHeader(wr io.Writer, batches []*Batch, seq uint64) error {
	if _, err := wr.Write(encodeBatchHeader(nil, seq, batchesLen(batches))); err != nil {
		return err
	}
	for _, batch := range batches {
		if _, err := wr.Write(batch.data); err != nil {
			return err
		}
	}
	return nil
}
