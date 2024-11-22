// Copyright (c) 2012, Suryandaru Triandana <syndtr@gmail.com>
// All rights reserved.
//
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package memdb provides in-memory key/value database implementation.
package memdb

import (
	"math/rand"
	"sync"

	"awesomeProject1/goleveldb/leveldb/comparer"
	"awesomeProject1/goleveldb/leveldb/errors"
	"awesomeProject1/goleveldb/leveldb/iterator"
	"awesomeProject1/goleveldb/leveldb/util"
)

// Common errors.
// 在memtable中，也是internalKey = uKey + seq N（标记新旧） + type（更新/删除）
// 比较规则：uKey不同，字典序大的大；uKey相同，seq N大的大；
var (
	ErrNotFound_     = errors.ErrNotFound
	ErrIterReleased_ = errors.New("leveldb/memdb: iterator released")
)

//const tMaxHeight = 12

type dbIter_ struct {
	util.BasicReleaser             //BasicReleaser provides basic implementation of Releaser and ReleaseSetter.
	p                  *DB_        //内存数据库
	slice              *util.Range //key的范围
	node               int
	forward            bool
	key, value         []byte
	err                error
}

func (i *dbIter_) fill(checkStart, checkLimit bool) bool {
	if i.node != 0 {
		n := i.p.nodeData[i.node]
		m := n + i.p.nodeData[i.node+nKey]
		i.key = i.p.kvData[n:m]
		if i.slice != nil {
			switch {
			case checkLimit && i.slice.Limit != nil && i.p.cmp.Compare(i.key, i.slice.Limit) >= 0:
				fallthrough
			case checkStart && i.slice.Start != nil && i.p.cmp.Compare(i.key, i.slice.Start) < 0:
				i.node = 0
				goto bail
			}
		}
		i.value = i.p.kvData[m : m+i.p.nodeData[i.node+nVal]]
		return true
	}
bail:
	i.key = nil
	i.value = nil
	return false
}

func (i *dbIter_) Valid() bool {
	return i.node != 0
}

func (i *dbIter_) First() bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	i.forward = true
	i.p.mu.RLock()
	defer i.p.mu.RUnlock()
	if i.slice != nil && i.slice.Start != nil {
		i.node, _ = i.p.findGE(i.slice.Start, false)
	} else {
		i.node = i.p.nodeData[nNext]
	}
	return i.fill(false, true)
}

func (i *dbIter_) Last() bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	i.forward = false
	i.p.mu.RLock()
	defer i.p.mu.RUnlock()
	if i.slice != nil && i.slice.Limit != nil {
		i.node = i.p.findLT(i.slice.Limit)
	} else {
		i.node = i.p.findLast()
	}
	return i.fill(true, false)
}

func (i *dbIter_) Seek(key []byte) bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	i.forward = true
	i.p.mu.RLock()
	defer i.p.mu.RUnlock()
	if i.slice != nil && i.slice.Start != nil && i.p.cmp.Compare(key, i.slice.Start) < 0 {
		key = i.slice.Start
	}
	i.node, _ = i.p.findGE(key, false)
	return i.fill(false, true)
}

func (i *dbIter_) Next() bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	if i.node == 0 {
		if !i.forward {
			return i.First()
		}
		return false
	}
	i.forward = true
	i.p.mu.RLock()
	defer i.p.mu.RUnlock()
	i.node = i.p.nodeData[i.node+nNext]
	return i.fill(false, true)
}

func (i *dbIter_) Prev() bool {
	if i.Released() {
		i.err = ErrIterReleased
		return false
	}

	if i.node == 0 {
		if i.forward {
			return i.Last()
		}
		return false
	}
	i.forward = false
	i.p.mu.RLock()
	defer i.p.mu.RUnlock()
	i.node = i.p.findLT(i.key)
	return i.fill(true, false)
}

func (i *dbIter_) Key() []byte {
	return i.key
}

func (i *dbIter_) Value() []byte {
	return i.value
}

func (i *dbIter_) Error() error { return i.err }

func (i *dbIter_) Release() {
	if !i.Released() {
		i.p = nil
		i.node = 0
		i.key = nil
		i.value = nil
		i.BasicReleaser.Release()
	}
}

//const (
//	nKV = iota
//	nKey
//	nVal
//	nHeight
//	nNext
//)

// DB is an in-memory key/value database.
type DB_ struct {
	cmp comparer.BasicComparer //比较器？
	rnd *rand.Rand             //随机数

	mu     sync.RWMutex //读写互斥锁
	kvData []byte       //每一条数据项的kv信息
	// Node data:
	// [0]         : KV offset 本节点kv数据在kvData中对应的偏移量
	// [1]         : Key length
	// [2]         : Value length
	// [3]         : Height //节点的层高
	// [3..height] : Next nodes //下一个节点的索引
	nodeData  []int //每个调表节点的连接信息。每个跳表节点占用一段连续的内存空间
	prevNode  [tMaxHeight]int
	maxHeight int
	n         int //kv对的数量
	kvSize    int
}

// 调表是否向上一层
func (p *DB_) randHeight() (h int) {
	const branching = 4
	h = 1
	//tMaxHeight为跳表最高层
	for h < tMaxHeight && p.rnd.Int()%branching == 0 {
		h++
	}
	return
}

// 检索大于等于key的相关信息
// Must hold RW-lock if prev == true, as it use shared prevNode slice.
func (p *DB_) findGE(key []byte, prev bool) (int, bool) {
	node := 0
	h := p.maxHeight - 1
	for {
		next := p.nodeData[node+nNext+h] //+h 表示从Node节点高度为h的层次开始查找
		cmp := 1
		if next != 0 {
			o := p.nodeData[next]
			cmp = p.cmp.Compare(p.kvData[o:o+p.nodeData[next+nKey]], key)
		}
		if cmp < 0 {
			// Keep searching in this list
			node = next
			//当前的kvData中key比目标key小，继续向前查找
		} else {
			//cmp >= 0, 表示当前值大于等于目标key，则目标key要么替代该位置，要么得插入在当前key的后面
			if prev {
				p.prevNode[h] = node
			} else if cmp == 0 {
				return next, true
			}
			if h == 0 {
				return next, cmp == 0
			}
			h--
		}
	}
}

func (p *DB_) findLT(key []byte) int {
	node := 0
	h := p.maxHeight - 1
	for {
		next := p.nodeData[node+nNext+h]
		o := p.nodeData[next]
		if next == 0 || p.cmp.Compare(p.kvData[o:o+p.nodeData[next+nKey]], key) >= 0 {
			if h == 0 {
				break
			}
			h--
		} else {
			node = next
		}
	}
	return node
}

func (p *DB_) findLast() int {
	node := 0
	h := p.maxHeight - 1
	for {
		next := p.nodeData[node+nNext+h]
		if next == 0 {
			if h == 0 {
				break
			}
			h--
		} else {
			node = next
		}
	}
	return node
}

// Put sets the value for the given key. It overwrites any previous value
// for that key; a DB is not a multi-map.
//
// It is safe to modify the contents of the arguments after Put returns.
func (p *DB_) Put(key []byte, value []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock() //互斥锁

	if node, exact := p.findGE(key, true); exact {
		kvOffset := len(p.kvData)
		p.kvData = append(p.kvData, key...)
		p.kvData = append(p.kvData, value...)
		p.nodeData[node] = kvOffset
		m := p.nodeData[node+nVal]
		p.nodeData[node+nVal] = len(value)
		p.kvSize += len(value) - m
		return nil
	}
	//插入新key
	h := p.randHeight()
	if h > p.maxHeight {
		for i := p.maxHeight; i < h; i++ {
			p.prevNode[i] = 0 //prevNode链接各个层级的node的第一个node
			//如果maxHight增加，则新增的preNode的节点指向最底层的数据
		}
		p.maxHeight = h
	}

	kvOffset := len(p.kvData)
	p.kvData = append(p.kvData, key...)
	p.kvData = append(p.kvData, value...)
	// Node
	node := len(p.nodeData)
	p.nodeData = append(p.nodeData, kvOffset, len(key), len(value), h) //插入索引node信息
	for i, n := range p.prevNode[:h] {                                 //p.prevNode的每个元素当前所的node 的key应该 <= newkey
		m := n + nNext + i
		p.nodeData = append(p.nodeData, p.nodeData[m]) //添加一个新的node节点，保证nodeData始终有足够的节点数存储层级索引
		//preNode记录所查找节点的所有前置节点位置
		//1.将newNode节点的前置节点的该层指向的位置复制给newNode的第h层
		//2.将newNode节点的前置节点的指向改为指向newNode
		p.nodeData[m] = node
	}

	p.kvSize += len(key) + len(value)
	p.n++
	return nil
}

// Delete deletes the value for the given key. It returns ErrNotFound if
// the DB does not contain the key.
//
// It is safe to modify the contents of the arguments after Delete returns.
func (p *DB_) Delete(key []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	node, exact := p.findGE(key, true)
	if !exact {
		return ErrNotFound
	}

	h := p.nodeData[node+nHeight]
	for i, n := range p.prevNode[:h] {
		m := n + nNext + i
		p.nodeData[m] = p.nodeData[p.nodeData[m]+nNext+i]
	}

	p.kvSize -= p.nodeData[node+nKey] + p.nodeData[node+nVal]
	p.n--
	return nil
}

// Contains returns true if the given key are in the DB.
//
// It is safe to modify the contents of the arguments after Contains returns.
func (p *DB_) Contains(key []byte) bool {
	p.mu.RLock()
	_, exact := p.findGE(key, false)
	p.mu.RUnlock()
	return exact
}

// Get gets the value for the given key. It returns error.ErrNotFound if the
// DB does not contain the key.
//
// The caller should not modify the contents of the returned slice, but
// it is safe to modify the contents of the argument after Get returns.
func (p *DB_) Get(key []byte) (value []byte, err error) {
	p.mu.RLock()
	if node, exact := p.findGE(key, false); exact {
		o := p.nodeData[node] + p.nodeData[node+nKey]
		value = p.kvData[o : o+p.nodeData[node+nVal]]
	} else {
		err = ErrNotFound
	}
	p.mu.RUnlock()
	return
}

// Find finds key/value pair whose key is greater than or equal to the
// given key. It returns ErrNotFound if the table doesn't contain
// such pair.
// 找的是大于或者等于给定key的kv键值对
// The caller should not modify the contents of the returned slice, but
// it is safe to modify the contents of the argument after Find returns.
func (p *DB_) Find(key []byte) (rkey, value []byte, err error) {
	p.mu.RLock()
	if node, _ := p.findGE(key, false); node != 0 {
		n := p.nodeData[node]
		m := n + p.nodeData[node+nKey]
		rkey = p.kvData[n:m]
		value = p.kvData[m : m+p.nodeData[node+nVal]]
	} else {
		err = ErrNotFound
	}
	p.mu.RUnlock()
	return
}

// NewIterator returns an iterator of the DB.
// The returned iterator is not safe for concurrent use, but it is safe to use
// multiple iterators concurrently, with each in a dedicated goroutine.
// It is also safe to use an iterator concurrently with modifying its
// underlying DB. However, the resultant key/value pairs are not guaranteed
// to be a consistent snapshot of the DB at a particular point in time.
//
// Slice allows slicing the iterator to only contains keys in the given
// range. A nil Range.Start is treated as a key before all keys in the
// DB. And a nil Range.Limit is treated as a key after all keys in
// the DB.
//
// WARNING: Any slice returned by interator (e.g. slice returned by calling
// Iterator.Key() or Iterator.Key() methods), its content should not be modified
// unless noted otherwise.
//
// The iterator must be released after use, by calling Release method.
//
// Also read Iterator documentation of the leveldb/iterator package.
// 迭代器
func (p *DB_) NewIterator(slice *util.Range) iterator.Iterator {
	return &dbIter_{p: p, slice: slice}
}

// Capacity returns keys/values buffer capacity.
// 返回的是buffer的容量
func (p *DB_) Capacity() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cap(p.kvData)
}

// Size returns sum of keys and values length. Note that deleted
// key/value will not be accounted for, but it will still consume
// the buffer, since the buffer is append only.
// 返回键和值长度的和。请注意，删除的键/值将不被考虑，但它仍然会消耗缓冲区，因为缓冲区只是追加的。
func (p *DB_) Size() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.kvSize
}

// Free returns keys/values free buffer before need to grow.
// 在需要增长之前，自由返回键/值的自由缓冲区
func (p *DB_) Free() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return cap(p.kvData) - len(p.kvData)
}

// Len returns the number of entries in the DB.
func (p *DB_) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.n
}

// Reset resets the DB to initial empty state. Allows reuse the buffer.
func (p *DB_) Reset() {
	p.mu.Lock()
	p.rnd = rand.New(rand.NewSource(0xdeadbeef))
	p.maxHeight = 1
	p.n = 0
	p.kvSize = 0
	p.kvData = p.kvData[:0]
	p.nodeData = p.nodeData[:nNext+tMaxHeight]
	p.nodeData[nKV] = 0
	p.nodeData[nKey] = 0
	p.nodeData[nVal] = 0
	p.nodeData[nHeight] = tMaxHeight
	for n := 0; n < tMaxHeight; n++ {
		p.nodeData[nNext+n] = 0
		p.prevNode[n] = 0
	}
	p.mu.Unlock()
}

// New creates a new initialized in-memory key/value DB. The capacity
// is the initial key/value buffer capacity. The capacity is advisory,
// not enforced.
//
// This DB is append-only, deleting an entry would remove entry node but not
// reclaim KV buffer.
//
// The returned DB instance is safe for concurrent use.
// 创建一个新的初始化的内存键/值DB。容量是初始的键/值缓冲容量。能力是建议性的，不是强制的。
//
// 该数据库是附加的，删除一个条目将删除条目节点，但不回收KV缓冲区。
//
// 返回的DB实例对于并发使用是安全的。
func New_(cmp comparer.BasicComparer, capacity int) *DB_ {
	p := &DB_{
		cmp:       cmp,
		rnd:       rand.New(rand.NewSource(0xdeadbeef)),
		maxHeight: 1,
		kvData:    make([]byte, 0, capacity),
		nodeData:  make([]int, 4+tMaxHeight),
	}
	p.nodeData[nHeight] = tMaxHeight
	return p
}
