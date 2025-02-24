// Copyright 2021 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// +build !race

package netpoll

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/bytedance/gopkg/lang/mcache"
)

// BinaryInplaceThreshold marks the minimum value of the nocopy slice length,
// which is the threshold to use copy to minimize overhead.
const BinaryInplaceThreshold = block4k

// LinkBufferCap that can be modified marks the minimum value of each node of LinkBuffer.
var LinkBufferCap = block4k

// NewLinkBuffer size defines the initial capacity, but there is no readable data.
func NewLinkBuffer(size ...int) *LinkBuffer {
	var buf = &LinkBuffer{}
	var l int
	if len(size) > 0 {
		l = size[0]
	}
	var node = newLinkBufferNode(l)
	buf.head, buf.read, buf.flush, buf.write = node, node, node, node
	return buf
}

// LinkBuffer implements ReadWriter.
type LinkBuffer struct {
	length     int32
	mallocSize int

	head  *linkBufferNode // release head
	read  *linkBufferNode // read head
	flush *linkBufferNode // malloc head
	write *linkBufferNode // malloc tail

	caches [][]byte // buf allocated by Next when cross-package, which should be freed when release
}

var _ Reader = &LinkBuffer{}
var _ Writer = &LinkBuffer{}

// Len implements Reader.
func (b *LinkBuffer) Len() int {
	l := atomic.LoadInt32(&b.length)
	return int(l)
}

// IsEmpty check if this LinkBuffer is empty.
func (b *LinkBuffer) IsEmpty() (ok bool) {
	return b.Len() == 0
}

// ------------------------------------------ implement zero-copy reader ------------------------------------------

// Next implements Reader.
func (b *LinkBuffer) Next(n int) (p []byte, err error) {
	// check whether enough or not.
	if b.Len() < n {
		return p, fmt.Errorf("link buffer next[%d] not enough", n)
	}
	b.recalLen(-n) // re-cal length

	// single node
	l := b.read.Len()
	if l >= n {
		return b.read.Next(n), nil
	}
	// multiple nodes
	var pIdx int
	if block1k < n && n <= mallocMax {
		p = malloc(n, n)
		b.caches = append(b.caches, p)
	} else {
		p = make([]byte, n)
	}
	for ack := n; ack > 0; ack = ack - l {
		l = b.read.Len()
		if l >= ack {
			pIdx += copy(p[pIdx:], b.read.Next(ack))
			break
		} else if l > 0 {
			pIdx += copy(p[pIdx:], b.read.Next(l))
		}
		b.read = b.read.next
	}
	_ = pIdx
	return p, nil
}

// Peek does not have an independent lifecycle, and there is no signal to
// indicate that Peek content can be released, so Peek will not introduce mcache for now.
func (b *LinkBuffer) Peek(n int) (p []byte, err error) {
	// check whether enough or not.
	if b.Len() < n {
		return p, fmt.Errorf("link buffer peek[%d] not enough", n)
	}
	var node = b.read
	// single node
	l := node.Len()
	if l >= n {
		return node.Peek(n), nil
	}
	// multiple nodes
	var pIdx int
	if block1k < n && n <= mallocMax {
		p = malloc(n, n)
		b.caches = append(b.caches, p)
	} else {
		p = make([]byte, n)
	}
	for ack := n; ack > 0; ack = ack - l {
		l = node.Len()
		if l >= ack {
			pIdx += copy(p[pIdx:], node.Peek(ack))
			break
		} else if l > 0 {
			pIdx += copy(p[pIdx:], node.Peek(l))
		}
		node = node.next
	}
	_ = pIdx
	return p, nil
}

// Skip implements Reader.
func (b *LinkBuffer) Skip(n int) (err error) {
	// check whether enough or not.
	if b.Len() < n {
		return fmt.Errorf("link buffer skip[%d] not enough", n)
	}
	b.recalLen(-n) // re-cal length

	var l int
	for ack := n; ack > 0; ack = ack - l {
		l = b.read.Len()
		if l >= ack {
			b.read.off += ack
			break
		}
		b.read = b.read.next
	}
	return nil
}

// Release the node that has been read.
// b.flush == nil indicates that this LinkBuffer is created by LinkBuffer.Slice
func (b *LinkBuffer) Release() (err error) {
	for b.read != b.flush && b.read.Len() == 0 {
		b.read = b.read.next
	}
	for b.head != b.read {
		node := b.head
		b.head = b.head.next
		node.Release()
	}
	for i := range b.caches {
		free(b.caches[i])
		b.caches[i] = nil
	}
	b.caches = b.caches[:0]
	return nil
}

// ReadString implements Reader.
func (b *LinkBuffer) ReadString(n int) (s string, err error) {
	// check whether enough or not.
	if b.Len() < n {
		return s, fmt.Errorf("link buffer read string[%d] not enough", n)
	}
	return unsafeSliceToString(b.readBinary(n)), nil
}

// ReadBinary implements Reader.
func (b *LinkBuffer) ReadBinary(n int) (p []byte, err error) {
	// check whether enough or not.
	if b.Len() < n {
		return p, fmt.Errorf("link buffer read binary[%d] not enough", n)
	}
	return b.readBinary(n), nil
}

// readBinary cannot use mcache, because the memory allocated by readBinary will not be recycled.
func (b *LinkBuffer) readBinary(n int) (p []byte) {
	b.recalLen(-n) // re-cal length

	// single node
	p = make([]byte, n)
	l := b.read.Len()
	if l >= n {
		copy(p, b.read.Next(n))
		return p
	}
	// multiple nodes
	var pIdx int
	for ack := n; ack > 0; ack = ack - l {
		l = b.read.Len()
		if l >= ack {
			pIdx += copy(p[pIdx:], b.read.Next(ack))
			break
		} else if l > 0 {
			pIdx += copy(p[pIdx:], b.read.Next(l))
		}
		b.read = b.read.next
	}
	_ = pIdx
	return p
}

// ReadByte implements Reader.
func (b *LinkBuffer) ReadByte() (p byte, err error) {
	// check whether enough or not.
	if b.Len() < 1 {
		return p, errors.New("link buffer read byte is empty")
	}
	b.recalLen(-1) // re-cal length
	for {
		if b.read.Len() >= 1 {
			return b.read.Next(1)[0], nil
		}
		b.read = b.read.next
	}
}

// Slice returns a new LinkBuffer, which is a zero-copy slice of this LinkBuffer,
// and only holds the ability of Reader.
//
// Slice will automatically execute a Release.
func (b *LinkBuffer) Slice(n int) (r Reader, err error) {
	// check whether enough or not.
	if b.Len() < n {
		return r, fmt.Errorf("link buffer readv[%d] not enough", n)
	}
	b.recalLen(-n) // re-cal length

	// just use for range
	p := &LinkBuffer{
		length: int32(n),
	}
	defer func() {
		// set to read-only
		p.flush = p.flush.next
		p.write = p.flush
	}()

	// single node
	l := b.read.Len()
	if l >= n {
		node := b.read.Refer(n)
		p.head, p.read, p.flush = node, node, node
		return p, nil
	}
	// multiple nodes
	node := b.read.Refer(l)
	b.read = b.read.next

	p.head, p.read, p.flush = node, node, node
	for ack := n - l; ack > 0; ack = ack - l {
		l = b.read.Len()
		if l >= ack {
			p.flush.next = b.read.Refer(ack)
			p.flush = p.flush.next
			break
		} else if l > 0 {
			p.flush.next = b.read.Refer(l)
			p.flush = p.flush.next
		}
		b.read = b.read.next
	}
	return p, b.Release()
}

// ------------------------------------------ implement zero-copy writer ------------------------------------------

// Malloc pre-allocates memory, which is not readable, and becomes readable data after submission(e.g. Flush).
func (b *LinkBuffer) Malloc(n int) (buf []byte, err error) {
	b.mallocSize += n
	b.growth(n)
	return b.write.Malloc(n), nil
}

// MallocLen implements Writer.
func (b *LinkBuffer) MallocLen() (length int) {
	return b.mallocSize
}

// MallocAck will keep the first n malloc bytes and discard the rest.
func (b *LinkBuffer) MallocAck(n int) (err error) {
	b.mallocSize = n
	b.write = b.flush

	var l int
	for ack := n; ack > 0; ack = ack - l {
		l = b.write.malloc - len(b.write.buf)
		if l >= ack {
			b.write.malloc = ack + len(b.write.buf)
			break
		}
		b.write = b.write.next
	}
	// discard the rest
	for node := b.write.next; node != nil; node = node.next {
		node.off, node.malloc, node.refer, node.buf = 0, 0, 1, node.buf[:0]
	}
	return nil
}

// Flush will submit all malloc data and must confirm that the allocated bytes have been correctly assigned.
func (b *LinkBuffer) Flush() (err error) {
	b.mallocSize = 0
	// FIXME: The tail node must not be larger than 8KB to prevent Out Of Memory.
	if cap(b.write.buf) > pagesize {
		b.write.next = newLinkBufferNode(0)
		b.write = b.write.next
	}
	var n int
	for node := b.flush; node != b.write.next; node = node.next {
		delta := node.malloc - len(node.buf)
		if delta > 0 {
			n += delta
			node.buf = node.buf[:node.malloc]
		}
	}
	b.flush = b.write
	// re-cal length
	b.recalLen(n)
	return nil
}

// Append implements Writer.
func (b *LinkBuffer) Append(w Writer) (n int, err error) {
	var buf, ok = w.(*LinkBuffer)
	if !ok {
		return 0, errors.New("unsupported writer which is not LinkBuffer")
	}
	return b.WriteBuffer(buf)
}

// WriteBuffer will not submit(e.g. Flush) data to ensure normal use of MallocLen.
// you must actively submit before read the data.
// The argument buf can't be used after calling WriteBuffer. (set it to nil)
func (b *LinkBuffer) WriteBuffer(buf *LinkBuffer) (n int, err error) {
	bufLen, bufMallocLen := buf.Len(), buf.MallocLen()
	if bufLen+bufMallocLen <= 0 {
		return 0, nil
	}
	b.write.next = buf.read
	b.write = buf.write

	// close buf, prevents reuse.
	for buf.head != buf.read {
		nd := buf.head
		buf.head = buf.head.next
		nd.Release()
	}
	for buf.write = buf.write.next; buf.write != nil; {
		nd := buf.write
		buf.write = buf.write.next
		nd.Release()
	}
	buf.length, buf.mallocSize, buf.head, buf.read, buf.flush, buf.write = 0, 0, nil, nil, nil, nil

	// DON'T MODIFY THE CODE BELOW UNLESS YOU KNOW WHAT YOU ARE DOING !
	//
	// You may encounter a chain of bugs and not be able to
	// find out within a week that they are caused by modifications here.
	//
	// After release buf, continue to adjust b.
	b.write.next = nil
	if bufLen > 0 {
		b.recalLen(bufLen)
	}
	b.mallocSize += bufMallocLen
	return n, nil
}

// WriteString implements Writer.
func (b *LinkBuffer) WriteString(s string) (n int, err error) {
	buf := unsafeStringToSlice(s)
	return b.WriteBinary(buf)
}

// WriteBinary implements Writer.
func (b *LinkBuffer) WriteBinary(p []byte) (n int, err error) {
	n = len(p)
	b.mallocSize += n

	// TODO: Verify that all nocopy is possible under mcache.
	if n > BinaryInplaceThreshold {
		// expand buffer directly with nocopy
		b.write.next = newLinkBufferNode(0)
		b.write = b.write.next
		b.write.buf, b.write.malloc = p[:0], n
		return n, nil
	}
	// here will copy
	b.growth(n)
	malloc := b.write.malloc
	b.write.malloc += n
	return copy(b.write.buf[malloc:b.write.malloc], p), nil
}

// WriteDirect cannot be mixed with WriteString or WriteBinary functions.
func (b *LinkBuffer) WriteDirect(p []byte, remainLen int) error {
	n := len(p)
	// find origin
	origin := b.flush
	malloc := b.mallocSize - remainLen // calculate the remaining malloc length
	for t := origin.malloc - len(origin.buf); t <= malloc; t = origin.malloc - len(origin.buf) {
		malloc -= t
		origin = origin.next
	}
	// Add the buf length of the original node
	malloc += len(origin.buf)

	// Create dataNode and newNode and insert them into the chain
	dataNode := newLinkBufferNode(0)
	dataNode.buf, dataNode.malloc = p[:0], n

	newNode := newLinkBufferNode(0)
	newNode.off = malloc
	newNode.buf = origin.buf[:malloc]
	newNode.malloc = origin.malloc
	newNode.readonly = false
	origin.malloc = malloc
	origin.readonly = true

	// link nodes
	dataNode.next = newNode
	newNode.next = origin.next
	origin.next = dataNode

	// adjust b.write
	for b.write.next != nil {
		b.write = b.write.next
	}

	b.mallocSize += n
	return nil
}

// WriteByte implements Writer.
func (b *LinkBuffer) WriteByte(p byte) (err error) {
	dst, err := b.Malloc(1)
	if len(dst) == 1 {
		dst[0] = p
	}
	return err
}

// Close will recycle all buffer.
func (b *LinkBuffer) Close() (err error) {
	atomic.StoreInt32(&b.length, 0)
	b.mallocSize = 0
	// just release all
	for node := b.head; node != nil; {
		nd := node
		node = node.next
		nd.Release()
	}
	// releaseLink(b.head, nil)
	// b.head, b.read, b.flush, b.write = emptyNode, emptyNode, emptyNode, emptyNode
	return nil
}

// ------------------------------------------ implement connection interface ------------------------------------------

// Bytes returns all the readable bytes of this LinkBuffer.
func (b *LinkBuffer) Bytes() []byte {
	node, flush := b.read, b.flush
	if node == flush {
		return node.buf[node.off:]
	}
	n := 0
	p := make([]byte, b.Len())
	for ; node != flush; node = node.next {
		if node.Len() > 0 {
			n += copy(p[n:], node.buf[node.off:])
		}
	}
	n += copy(p[n:], flush.buf[flush.off:])
	return p[:n]
}

// GetBytes will read and fill the slice p as much as possible.
func (b *LinkBuffer) GetBytes(p [][]byte) (vs [][]byte) {
	node, flush := b.read, b.flush
	var i int
	for i = 0; node != flush && i < len(p); node = node.next {
		if node.Len() > 0 {
			p[i] = node.buf[node.off:]
			i++
		}
	}
	if i < len(p) {
		p[i] = flush.buf[flush.off:]
		i++
	}
	return p[:i]
}

// Book will grow and fill the slice p greater than min.
func (b *LinkBuffer) Book(min int, p [][]byte) (vs [][]byte) {
	var i, l int
	for {
		l = cap(b.write.buf) - b.write.malloc
		if l > 0 {
			p[i] = b.write.Malloc(l)
			i++
			min -= l
			if min <= 0 || i == len(p) {
				break
			}
		}
		if b.write.next == nil {
			b.write.next = newLinkBufferNode(min)
		}
		b.write = b.write.next
	}
	return p[:i]
}

// BookAck will ack the first n malloc bytes and discard the rest.
func (b *LinkBuffer) BookAck(n int, isEnd bool) (err error) {
	var l int
	for ack := n; ack > 0; ack = ack - l {
		l = b.flush.malloc - len(b.flush.buf)
		if l >= ack {
			b.flush.malloc = ack + len(b.flush.buf)
			b.flush.buf = b.flush.buf[:b.flush.malloc]
			break
		} else if l > 0 {
			b.flush.buf = b.flush.buf[:b.flush.malloc]
		}
		b.flush = b.flush.next
	}
	// discard the rest & write = flush
	for node := b.flush.next; node != nil; node = node.next {
		node.off, node.malloc, node.refer, node.buf = 0, 0, 1, node.buf[:0]
	}

	// FIXME: The tail node must not be larger than 8KB to prevent Out Of Memory.
	if isEnd && cap(b.flush.buf) > pagesize {
		if b.flush.next == nil {
			b.flush.next = newLinkBufferNode(0)
		}
		b.flush = b.flush.next
	}
	b.write = b.flush

	// re-cal length
	b.recalLen(n)
	return nil
}

// Reset resets the buffer to be empty,
// but it retains the underlying storage for use by future writes.
// Reset is the same as Truncate(0).
// func (b *LinkBuffer) Reset() {
// 	atomic.StoreInt32(&b.length, 0)
//  b.mallocSize = 0
// 	releaseLink(b.head, nil)
// 	node := linkedPool.Get().(*linkBufferNode)
// 	b.head, b.read, b.flush, b.write = node, node, node, node
// }

// recalLen re-calculate the length
func (b *LinkBuffer) recalLen(delta int) (err error) {
	atomic.AddInt32(&b.length, int32(delta))
	return nil
}

// ------------------------------------------ implement link node ------------------------------------------

// newLinkBufferNode create or reuse linkBufferNode.
// Nodes with size <= 0 are marked as readonly, which means the node.buf is not allocated by this mcache.
func newLinkBufferNode(size int) *linkBufferNode {
	var node = linkedPool.Get().(*linkBufferNode)
	if size <= 0 {
		node.readonly = true
		return node
	}
	if size < LinkBufferCap {
		size = LinkBufferCap
	}
	node.buf = malloc(0, size)
	return node
}

var linkedPool = sync.Pool{
	New: func() interface{} {
		return &linkBufferNode{
			refer: 1, // 自带 1 引用
		}
	},
}

type linkBufferNode struct {
	buf      []byte          // buffer
	off      int             // read-offset
	malloc   int             // write-offset
	refer    int32           // reference count
	readonly bool            // read-only node, introduced by Refer, WriteString, WriteBinary, etc., default false
	origin   *linkBufferNode // the root node of the extends
	next     *linkBufferNode // the next node of the linked buffer
}

func (node *linkBufferNode) Len() (l int) {
	return len(node.buf) - node.off
}

func (node *linkBufferNode) IsEmpty() (ok bool) {
	return node.off == len(node.buf)
}

//
// func (node *linkBufferNode) Reset() (err error) {
// 	node.off, node.malloc, node.refer, node.next, node.origin = 0, 0, 1, nil, nil
// 	node.buf = node.buf[:0]
// 	return nil
// }
//
// func (node *linkBufferNode) Close() (err error) {
// 	node.off, node.malloc, node.refer = 0, 0, 0
// 	node.next, node.origin, node.buf = nil, nil, zeroSlice
// 	return nil
// }

func (node *linkBufferNode) Next(n int) (p []byte) {
	off := node.off
	node.off += n
	return node.buf[off:node.off]
}

func (node *linkBufferNode) Peek(n int) (p []byte) {
	return node.buf[node.off : node.off+n]
}

func (node *linkBufferNode) Malloc(n int) (buf []byte) {
	malloc := node.malloc
	node.malloc += n
	return node.buf[malloc:node.malloc]
}

// Refer holds a reference count at the same time as Next, and releases the real buffer after Release.
// The node obtained by Refer is read-only.
func (node *linkBufferNode) Refer(n int) (p *linkBufferNode) {
	p = newLinkBufferNode(0)
	p.buf = node.Next(n)

	if node.origin != nil {
		p.origin = node.origin
	} else {
		p.origin = node
	}
	atomic.AddInt32(&p.origin.refer, 1)
	return p
}

// Release consists of two parts:
// 1. reduce the reference count of itself and origin.
// 2. recycle the buf when the reference count is 0.
func (node *linkBufferNode) Release() (err error) {
	if node.origin != nil {
		node.origin.Release()
	}
	// release self
	if atomic.AddInt32(&node.refer, -1) == 0 {
		node.off, node.malloc, node.refer, node.origin, node.next = 0, 0, 1, nil, nil
		// readonly nodes cannot recycle node.buf, other node.buf are recycled to mcache.
		if node.readonly {
			node.readonly = false
		} else {
			free(node.buf)
		}
		node.buf = nil
		linkedPool.Put(node)
	}
	return nil
}

// ------------------------------------------ private function ------------------------------------------

// growth directly create the next node, when b.write is not enough.
func (b *LinkBuffer) growth(n int) {
	if n <= 0 {
		return
	}
	// Must skip read-only node.
	for b.write.readonly || cap(b.write.buf)-b.write.malloc < n {
		if b.write.next == nil {
			b.write.next = newLinkBufferNode(n)
			b.write = b.write.next
			return
		}
		b.write = b.write.next
	}
}

// zero-copy slice convert to string
func unsafeSliceToString(b []byte) string {
	return *(*string)(unsafe.Pointer(&b))
}

// zero-copy slice convert to string
func unsafeStringToSlice(s string) (b []byte) {
	p := unsafe.Pointer((*reflect.StringHeader)(unsafe.Pointer(&s)).Data)
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	hdr.Data = uintptr(p)
	hdr.Cap = len(s)
	hdr.Len = len(s)
	return b
}

// mallocMax is 8MB
const mallocMax = block8k * block1k

// malloc limits the cap of the buffer from mcache.
func malloc(size, capacity int) []byte {
	if capacity > mallocMax {
		return make([]byte, size, capacity)
	}
	return mcache.Malloc(size, capacity)
}

// free limits the cap of the buffer from mcache.
func free(buf []byte) {
	if cap(buf) > mallocMax {
		return
	}
	mcache.Free(buf)
}
