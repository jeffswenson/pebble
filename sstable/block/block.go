// Copyright 2024 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package block

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"github.com/cespare/xxhash/v2"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/crc"
)

// Handle is the file offset and length of a block.
type Handle struct {
	Offset, Length uint64
}

// EncodeVarints encodes the block handle into dst using a variable-width
// encoding and returns the number of bytes written.
func (h Handle) EncodeVarints(dst []byte) int {
	n := binary.PutUvarint(dst, h.Offset)
	m := binary.PutUvarint(dst[n:], h.Length)
	return n + m
}

// HandleWithProperties is used for data blocks and first/lower level index
// blocks, since they can be annotated using BlockPropertyCollectors.
type HandleWithProperties struct {
	Handle
	Props []byte
}

// EncodeVarints encodes the block handle and properties into dst using a
// variable-width encoding and returns the number of bytes written.
func (h HandleWithProperties) EncodeVarints(dst []byte) []byte {
	n := h.Handle.EncodeVarints(dst)
	dst = append(dst[:n], h.Props...)
	return dst
}

// DecodeHandle returns the block handle encoded in a variable-width encoding at
// the start of src, as well as the number of bytes it occupies. It returns zero
// if given invalid input. A block handle for a data block or a first/lower
// level index block should not be decoded using DecodeHandle since the caller
// may validate that the number of bytes decoded is equal to the length of src,
// which will be false if the properties are not decoded. In those cases the
// caller should use DecodeHandleWithProperties.
func DecodeHandle(src []byte) (Handle, int) {
	offset, n := binary.Uvarint(src)
	length, m := binary.Uvarint(src[n:])
	if n == 0 || m == 0 {
		return Handle{}, 0
	}
	return Handle{Offset: offset, Length: length}, n + m
}

// DecodeHandleWithProperties returns the block handle and properties encoded in
// a variable-width encoding at the start of src. src needs to be exactly the
// length that was encoded. This method must be used for data block and
// first/lower level index blocks. The properties in the block handle point to
// the bytes in src.
func DecodeHandleWithProperties(src []byte) (HandleWithProperties, error) {
	bh, n := DecodeHandle(src)
	if n == 0 {
		return HandleWithProperties{}, errors.Errorf("invalid block.Handle")
	}
	return HandleWithProperties{
		Handle: bh,
		Props:  src[n:],
	}, nil
}

// TrailerLen is the length of the trailer at the end of a block.
const TrailerLen = 5

// Trailer is the trailer at the end of a block, encoding the block type
// (compression) and a checksum.
type Trailer = [TrailerLen]byte

// MakeTrailer constructs a trailer from a block type and a checksum.
func MakeTrailer(blockType byte, checksum uint32) (t Trailer) {
	t[0] = blockType
	binary.LittleEndian.PutUint32(t[1:5], checksum)
	return t
}

// ChecksumType specifies the checksum used for blocks.
type ChecksumType byte

// The available checksum types. These values are part of the durable format and
// should not be changed.
const (
	ChecksumTypeNone     ChecksumType = 0
	ChecksumTypeCRC32c   ChecksumType = 1
	ChecksumTypeXXHash   ChecksumType = 2
	ChecksumTypeXXHash64 ChecksumType = 3
)

// String implements fmt.Stringer.
func (t ChecksumType) String() string {
	switch t {
	case ChecksumTypeCRC32c:
		return "crc32c"
	case ChecksumTypeNone:
		return "none"
	case ChecksumTypeXXHash:
		return "xxhash"
	case ChecksumTypeXXHash64:
		return "xxhash64"
	default:
		panic(errors.Newf("sstable: unknown checksum type: %d", t))
	}
}

// A Checksummer calculates checksums for blocks.
type Checksummer struct {
	Type     ChecksumType
	xxHasher *xxhash.Digest
}

// Checksum computes a checksum over the provided block and block type.
func (c *Checksummer) Checksum(block []byte, blockType []byte) (checksum uint32) {
	// Calculate the checksum.
	switch c.Type {
	case ChecksumTypeCRC32c:
		checksum = crc.New(block).Update(blockType).Value()
	case ChecksumTypeXXHash64:
		if c.xxHasher == nil {
			c.xxHasher = xxhash.New()
		} else {
			c.xxHasher.Reset()
		}
		c.xxHasher.Write(block)
		c.xxHasher.Write(blockType)
		checksum = uint32(c.xxHasher.Sum64())
	default:
		panic(errors.Newf("unsupported checksum type: %d", c.Type))
	}
	return checksum
}

// DataBlockIterator is a type constraint for implementations of block iterators
// over data blocks. It's currently satisifed by the *rowblk.Iter type.
type DataBlockIterator interface {
	base.InternalIterator

	// Handle returns the handle to the block.
	Handle() BufferHandle
	// InitHandle initializes the block from the provided buffer handle.
	InitHandle(base.Compare, base.Split, BufferHandle, IterTransforms) error
	// Valid returns true if the iterator is currently positioned at a valid KV.
	Valid() bool
	// KV returns the key-value pair at the current iterator position. The
	// iterator must be Valid().
	KV() *base.InternalKV
	// IsLowerBound returns true if all keys produced by this iterator are >= the
	// given key. The function is best effort; false negatives are allowed.
	//
	// If IsLowerBound is true then Compare(First().UserKey, k) >= 0.
	//
	// If the iterator produces no keys (i.e. First() is nil), IsLowerBound can
	// return true for any key.
	IsLowerBound(k []byte) bool
	// Invalidate invalidates the block iterator, removing references to the
	// block it was initialized with. The iterator may continue to be used after
	// a call to Invalidate, but all positioning methods should return false.
	// Valid() must also return false.
	Invalidate()
	// IsDataInvalidated returns true when the iterator has been invalidated
	// using an Invalidate call.
	//
	// NB: this is different from Valid which indicates whether the current *KV*
	// is valid.
	IsDataInvalidated() bool
}

// IndexBlockIterator is an interface for implementations of block
// iterators over index blocks. It's currently satisifed by the
// *rowblk.IndexIter type.
type IndexBlockIterator interface {
	// Init initializes the block iterator from the provided block.
	Init(base.Compare, base.Split, []byte, IterTransforms) error
	// InitHandle initializes an iterator from the provided block handle.
	InitHandle(base.Compare, base.Split, BufferHandle, IterTransforms) error
	// Valid returns true if the iterator is currently positioned at a valid
	// block handle.
	Valid() bool
	// IsDataInvalidated returns true when the iterator has been invalidated
	// using an Invalidate call.
	//
	// NB: this is different from Valid which indicates whether the iterator is
	// currently positioned over a valid block entry.
	IsDataInvalidated() bool
	// Invalidate invalidates the block iterator, removing references to the
	// block it was initialized with. The iterator may continue to be used after
	// a call to Invalidate, but all positioning methods should return false.
	// Valid() must also return false.
	Invalidate()
	// Handle returns the underlying block buffer handle, if the iterator was
	// initialized with one.
	Handle() BufferHandle
	// Separator returns the separator at the iterator's current position. The
	// iterator must be positioned at a valid row. A Separator is a user key
	// guaranteed to be greater than or equal to every key contained within the
	// referenced block(s).
	Separator() []byte
	// BlockHandleWithProperties decodes the block handle with any encoded
	// properties at the iterator's current position.
	BlockHandleWithProperties() (HandleWithProperties, error)
	// SeekGE seeks the index iterator to the first block entry with a separator
	// key greater or equal to the given key. If it returns true, the iterator
	// is positioned over the first block that might contain the key [key], and
	// following blocks have keys ≥ Separator(). It returns false if the seek
	// key is greater than all index block separators.
	SeekGE(key []byte) bool
	// First seeks index iterator to the first block entry. It returns false if
	// the index block is empty.
	First() bool
	// Last seeks index iterator to the last block entry. It returns false if
	// the index block is empty.
	Last() bool
	// Next steps the index iterator to the next block entry. It returns false
	// if the index block is exhausted in the forward direction. A call to Next
	// while already exhausted in the forward direction is a no-op.
	Next() bool
	// Prev steps the index iterator to the previous block entry. It returns
	// false if the index block is exhausted in the reverse direction. A call to
	// Prev while already exhausted in the reverse direction is a no-op.
	Prev() bool
	// Close closes the iterator, releasing any resources it holds.
	Close() error
}

// IterTransforms allow on-the-fly transformation of data at iteration time.
//
// These transformations could in principle be implemented as block transforms
// (at least for non-virtual sstables), but applying them during iteration is
// preferable.
type IterTransforms struct {
	// SyntheticSeqNum, if set, overrides the sequence number in all keys. It is
	// set if the sstable was ingested or it is foreign.
	SyntheticSeqNum SyntheticSeqNum
	// HideObsoletePoints, if true, skips over obsolete points during iteration.
	// This is the norm when the sstable is foreign or the largest sequence number
	// of the sstable is below the one we are reading.
	HideObsoletePoints bool
	SyntheticPrefix    SyntheticPrefix
	SyntheticSuffix    SyntheticSuffix
}

// NoTransforms is the default value for IterTransforms.
var NoTransforms = IterTransforms{}

// FragmentIterTransforms allow on-the-fly transformation of range deletion or
// range key data at iteration time.
type FragmentIterTransforms struct {
	SyntheticSeqNum SyntheticSeqNum
	// ElideSameSeqNum, if true, returns only the first-occurring (in forward
	// order) keyspan.Key for each sequence number.
	ElideSameSeqNum bool
	SyntheticPrefix SyntheticPrefix
	SyntheticSuffix SyntheticSuffix
}

// NoFragmentTransforms is the default value for IterTransforms.
var NoFragmentTransforms = FragmentIterTransforms{}

// SyntheticSeqNum is used to override all sequence numbers in a table. It is
// set to a non-zero value when the table was created externally and ingested
// whole.
type SyntheticSeqNum base.SeqNum

// NoSyntheticSeqNum is the default zero value for SyntheticSeqNum, which
// disables overriding the sequence number.
const NoSyntheticSeqNum SyntheticSeqNum = 0

// SyntheticSuffix will replace every suffix of every point key surfaced during
// block iteration. A synthetic suffix can be used if:
//  1. no two keys in the sst share the same prefix; and
//  2. pebble.Compare(prefix + replacementSuffix, prefix + originalSuffix) < 0,
//     for all keys in the backing sst which have a suffix (i.e. originalSuffix
//     is not empty).
//
// Range dels are not supported when synthetic suffix is used.
//
// For range keys, the synthetic suffix applies to the suffix that is part of
// RangeKeySet - if it is non-empty, it is replaced with the SyntheticSuffix.
// RangeKeyUnset keys are not supported when a synthetic suffix is used.
type SyntheticSuffix []byte

// IsSet returns true if the synthetic suffix is not enpty.
func (ss SyntheticSuffix) IsSet() bool {
	return len(ss) > 0
}

// SyntheticPrefix represents a byte slice that is implicitly prepended to every
// key in a file being read or accessed by a reader. Note that since the byte
// slice is prepended to every KV rather than replacing a byte prefix, the
// result of prepending the synthetic prefix must be a full, valid key while the
// partial key physically stored within the sstable need not be a valid key
// according to user key semantics.
//
// Note that elsewhere we use the language of 'prefix' to describe the user key
// portion of a MVCC key, as defined by the Comparer's base.Split method. The
// SyntheticPrefix is related only in that it's a byte prefix that is
// incorporated into the logical MVCC prefix.
//
// The table's bloom filters are constructed only on the partial keys physically
// stored in the table, but interactions with the file including seeks and
// reads will all behave as if the file had been constructed from keys that
// include the synthetic prefix. Note that all Compare operations will act on a
// partial key (before any prepending), so the Comparer must support comparing
// these partial keys.
//
// The synthetic prefix will never modify key metadata stored in the key suffix.
//
// NB: Since this transformation currently only applies to point keys, a block
// with range keys cannot be iterated over with a synthetic prefix.
type SyntheticPrefix []byte

// IsSet returns true if the synthetic prefix is not enpty.
func (sp SyntheticPrefix) IsSet() bool {
	return len(sp) > 0
}

// Apply prepends the synthetic prefix to a key.
func (sp SyntheticPrefix) Apply(key []byte) []byte {
	res := make([]byte, 0, len(sp)+len(key))
	res = append(res, sp...)
	res = append(res, key...)
	return res
}

// Invert removes the synthetic prefix from a key.
func (sp SyntheticPrefix) Invert(key []byte) []byte {
	res, ok := bytes.CutPrefix(key, sp)
	if !ok {
		panic(fmt.Sprintf("unexpected prefix: %s", key))
	}
	return res
}
