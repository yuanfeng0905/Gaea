// Copyright 2024 The Gaea Authors. All Rights Reserved.
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

package bucketpool

import (
	"math/bits"
	"sync"
)

type sizedPool struct {
	size int
	pool sync.Pool
}

func newSizedPool(size int) *sizedPool {
	return &sizedPool{
		size: size,
		pool: sync.Pool{
			New: func() any { return makeSlicePointer(size) },
		},
	}
}

// Pool is actually multiple pools which store buffers of specific size.
// i.e. it can be three pools which return buffers 32K, 64K and 128K.
type Pool struct {
	minSize int
	maxSize int
	pools   []*sizedPool
}

// New returns Pool which has buckets from minSize to maxSize.
// Buckets increase with the power of two, i.e with multiplier 2: [2b, 4b, 16b, ... , 1024b]
// Last pool will always be capped to maxSize.
func New(minSize, maxSize int) *Pool {
	if maxSize < minSize {
		panic("maxSize can't be less than minSize")
	}
	const multiplier = 2
	var pools []*sizedPool
	curSize := minSize
	for curSize < maxSize {
		pools = append(pools, newSizedPool(curSize))
		curSize *= multiplier
	}
	pools = append(pools, newSizedPool(maxSize))
	return &Pool{
		minSize: minSize,
		maxSize: maxSize,
		pools:   pools,
	}
}

func (p *Pool) findPool(size int) *sizedPool {
	if size > p.maxSize {
		return nil
	}
	div, rem := bits.Div64(0, uint64(size), uint64(p.minSize))
	idx := bits.Len64(div)
	if rem == 0 && div != 0 && (div&(div-1)) == 0 {
		idx = idx - 1
	}
	return p.pools[idx]
}

// Get returns pointer to []byte which has len size.
// If there is no bucket with buffers >= size, slice will be allocated.
func (p *Pool) Get(size int) *[]byte {
	sp := p.findPool(size)
	if sp == nil {
		return makeSlicePointer(size)
	}
	buf := sp.pool.Get().(*[]byte)
	*buf = (*buf)[:size]
	return buf
}

// Put returns pointer to slice to some bucket. Discards slice for which there is no bucket
func (p *Pool) Put(b *[]byte) {
	sp := p.findPool(cap(*b))
	if sp == nil {
		return
	}
	*b = (*b)[:cap(*b)]
	sp.pool.Put(b)
}

func makeSlicePointer(size int) *[]byte {
	data := make([]byte, size)
	return &data
}
