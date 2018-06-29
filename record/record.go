// Copyright 2011 The LevelDB-Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package record reads and writes sequences of records. Each record is a stream
// of bytes that completes before the next record starts.
//
// When reading, call Next to obtain an io.Reader for the next record. Next will
// return io.EOF when there are no more records. It is valid to call Next
// without reading the current record to exhaustion.
//
// When writing, call Next to obtain an io.Writer for the next record. Calling
// Next finishes the current record. Call Close to finish the final record.
//
// Optionally, call Flush to finish the current record and flush the underlying
// writer without starting a new record. To start a new record after flushing,
// call Next.
//
// Neither Readers or Writers are safe to use concurrently.
//
// Example code:
//	func read(r io.Reader) ([]string, error) {
//		var ss []string
//		records := record.NewReader(r)
//		for {
//			rec, err := records.Next()
//			if err == io.EOF {
//				break
//			}
//			if err != nil {
//				log.Printf("recovering from %v", err)
//				r.Recover()
//				continue
//			}
//			s, err := ioutil.ReadAll(rec)
//			if err != nil {
//				log.Printf("recovering from %v", err)
//				r.Recover()
//				continue
//			}
//			ss = append(ss, string(s))
//		}
//		return ss, nil
//	}
//
//	func write(w io.Writer, ss []string) error {
//		records := record.NewWriter(w)
//		for _, s := range ss {
//			rec, err := records.Next()
//			if err != nil {
//				return err
//			}
//			if _, err := rec.Write([]byte(s)), err != nil {
//				return err
//			}
//		}
//		return records.Close()
//	}
//
// The wire format is that the stream is divided into 32KiB blocks, and each
// block contains a number of tightly packed chunks. Chunks cannot cross block
// boundaries. The last block may be shorter than 32 KiB. Any unused bytes in a
// block must be zero.
//
// A record maps to one or more chunks. Each chunk has a 7 byte header (a 4
// byte checksum, a 2 byte little-endian uint16 length, and a 1 byte chunk type)
// followed by a payload. The checksum is over the chunk type and the payload.
//
// There are four chunk types: whether the chunk is the full record, or the
// first, middle or last chunk of a multi-chunk record. A multi-chunk record
// has one first chunk, zero or more middle chunks, and one last chunk.
//
// The wire format allows for limited recovery in the face of data corruption:
// on a format error (such as a checksum mismatch), the reader moves to the
// next block and looks for the next full or first chunk.
package record // import "github.com/petermattis/pebble/record"

// The C++ Level-DB code calls this the log, but it has been renamed to record
// to avoid clashing with the standard log package, and because it is generally
// useful outside of logging. The C++ code also uses the term "physical record"
// instead of "chunk", but "chunk" is shorter and less confusing.

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/petermattis/pebble/crc"
)

// These constants are part of the wire format and should not be changed.
const (
	fullChunkType   = 1
	firstChunkType  = 2
	middleChunkType = 3
	lastChunkType   = 4
)

const (
	blockSize     = 32 * 1024
	blockSizeMask = blockSize - 1
	headerSize    = 7
)

var (
	// ErrNotAnIOSeeker is returned if the io.Reader underlying a Reader does not implement io.Seeker.
	ErrNotAnIOSeeker = errors.New("pebble/record: reader does not implement io.Seeker")

	// ErrNoLastRecord is returned if LastRecordOffset is called and there is no previous record.
	ErrNoLastRecord = errors.New("pebble/record: no last record exists")
)

type flusher interface {
	Flush() error
}

type syncer interface {
	Sync() error
}

// Reader reads records from an underlying io.Reader.
type Reader struct {
	// r is the underlying reader.
	r io.Reader
	// seq is the sequence number of the current record.
	seq int
	// buf[i:j] is the unread portion of the current chunk's payload.
	// The low bound, i, excludes the chunk header.
	i, j int
	// n is the number of bytes of buf that are valid. Once reading has started,
	// only the final block can have n < blockSize.
	n int
	// started is whether Next has been called at all.
	started bool
	// recovering is true when recovering from corruption.
	recovering bool
	// last is whether the current chunk is the last chunk of the record.
	last bool
	// err is any accumulated error.
	err error
	// buf is the buffer.
	buf [blockSize]byte
}

// NewReader returns a new reader.
func NewReader(r io.Reader) *Reader {
	return &Reader{
		r: r,
	}
}

// nextChunk sets r.buf[r.i:r.j] to hold the next chunk's payload, reading the
// next block into the buffer if necessary.
func (r *Reader) nextChunk(wantFirst bool) error {
	for {
		if r.j+headerSize <= r.n {
			checksum := binary.LittleEndian.Uint32(r.buf[r.j+0 : r.j+4])
			length := binary.LittleEndian.Uint16(r.buf[r.j+4 : r.j+6])
			chunkType := r.buf[r.j+6]

			if checksum == 0 && length == 0 && chunkType == 0 {
				if wantFirst || r.recovering {
					// Skip the rest of the block, if it looks like it is all
					// zeroes. This is common if the record file was created
					// via mmap.
					//
					// Set r.err to be an error so r.Recover actually recovers.
					r.err = errors.New("pebble/record: block appears to be zeroed")
					r.Recover()
					continue
				}
				return errors.New("pebble/record: invalid chunk")
			}

			r.i = r.j + headerSize
			r.j = r.j + headerSize + int(length)
			if r.j > r.n {
				if r.recovering {
					r.Recover()
					continue
				}
				return errors.New("pebble/record: invalid chunk (length overflows block)")
			}
			if checksum != crc.New(r.buf[r.i-1:r.j]).Value() {
				if r.recovering {
					r.Recover()
					continue
				}
				return errors.New("pebble/record: invalid chunk (checksum mismatch)")
			}
			if wantFirst {
				if chunkType != fullChunkType && chunkType != firstChunkType {
					continue
				}
			}
			r.last = chunkType == fullChunkType || chunkType == lastChunkType
			r.recovering = false
			return nil
		}
		if r.n < blockSize && r.started {
			if r.j != r.n {
				return io.ErrUnexpectedEOF
			}
			return io.EOF
		}
		n, err := io.ReadFull(r.r, r.buf[:])
		if err != nil && err != io.ErrUnexpectedEOF {
			return err
		}
		r.i, r.j, r.n = 0, 0, n
	}
}

// Next returns a reader for the next record. It returns io.EOF if there are no
// more records. The reader returned becomes stale after the next Next call,
// and should no longer be used.
func (r *Reader) Next() (io.Reader, error) {
	r.seq++
	if r.err != nil {
		return nil, r.err
	}
	r.i = r.j
	r.err = r.nextChunk(true)
	if r.err != nil {
		return nil, r.err
	}
	r.started = true
	return singleReader{r, r.seq}, nil
}

// Recover clears any errors read so far, so that calling Next will start
// reading from the next good 32KiB block. If there are no such blocks, Next
// will return io.EOF. Recover also marks the current reader, the one most
// recently returned by Next, as stale. If Recover is called without any
// prior error, then Recover is a no-op.
func (r *Reader) Recover() {
	if r.err == nil {
		return
	}
	r.recovering = true
	r.err = nil
	// Discard the rest of the current block.
	r.i, r.j, r.last = r.n, r.n, false
	// Invalidate any outstanding singleReader.
	r.seq++
	return
}

// SeekRecord seeks in the underlying io.Reader such that calling r.Next
// returns the record whose first chunk header starts at the provided offset.
// Its behavior is undefined if the argument given is not such an offset, as
// the bytes at that offset may coincidentally appear to be a valid header.
//
// It returns ErrNotAnIOSeeker if the underlying io.Reader does not implement
// io.Seeker.
//
// SeekRecord will fail and return an error if the Reader previously
// encountered an error, including io.EOF. Such errors can be cleared by
// calling Recover. Calling SeekRecord after Recover will make calling Next
// return the record at the given offset, instead of the record at the next
// good 32KiB block as Recover normally would. Calling SeekRecord before
// Recover has no effect on Recover's semantics other than changing the
// starting point for determining the next good 32KiB block.
//
// The offset is always relative to the start of the underlying io.Reader, so
// negative values will result in an error as per io.Seeker.
func (r *Reader) SeekRecord(offset int64) error {
	r.seq++
	if r.err != nil {
		return r.err
	}

	s, ok := r.r.(io.Seeker)
	if !ok {
		return ErrNotAnIOSeeker
	}

	// Only seek to an exact block offset.
	c := int(offset & blockSizeMask)
	if _, r.err = s.Seek(offset&^blockSizeMask, io.SeekStart); r.err != nil {
		return r.err
	}

	// Clear the state of the internal reader.
	r.i, r.j, r.n = 0, 0, 0
	r.started, r.recovering, r.last = false, false, false
	if r.err = r.nextChunk(false); r.err != nil {
		return r.err
	}

	// Now skip to the offset requested within the block. A subsequent
	// call to Next will return the block at the requested offset.
	r.i, r.j = c, c

	return nil
}

type singleReader struct {
	r   *Reader
	seq int
}

func (x singleReader) Read(p []byte) (int, error) {
	r := x.r
	if r.seq != x.seq {
		return 0, errors.New("pebble/record: stale reader")
	}
	if r.err != nil {
		return 0, r.err
	}
	for r.i == r.j {
		if r.last {
			return 0, io.EOF
		}
		if r.err = r.nextChunk(false); r.err != nil {
			return 0, r.err
		}
	}
	n := copy(p, r.buf[r.i:r.j])
	r.i += n
	return n, nil
}

// Writer writes records to an underlying io.Writer.
type Writer struct {
	// w is the underlying writer.
	w io.Writer
	// f is w as a flusher.
	f flusher
	// s is w as a syncer.
	s syncer
	// buf[i:j] is the bytes that will become the current chunk.
	// The low bound, i, includes the chunk header.
	i, j int
	// buf[:written] has already been written to w.
	// written is zero unless Flush has been called.
	written int
	// baseOffset is the base offset in w at which writing started. If
	// w implements io.Seeker, it's relative to the start of w, 0 otherwise.
	baseOffset int64
	// blockNumber is the zero based block number currently held in buf.
	blockNumber int64
	// lastRecordOffset is the offset in w where the last record was
	// written (including the chunk header). It is a relative offset to
	// baseOffset, thus the absolute offset of the last record is
	// baseOffset + lastRecordOffset.
	lastRecordOffset int64
	// first is whether the current chunk is the first chunk of the record.
	first bool
	// pending is whether a chunk is buffered but not yet written.
	pending bool
	// err is any accumulated error.
	err error
	// buf is the buffer.
	buf [blockSize]byte
}

// NewWriter returns a new Writer.
func NewWriter(w io.Writer) *Writer {
	f, _ := w.(flusher)
	s, _ := w.(syncer)

	var o int64
	if s, ok := w.(io.Seeker); ok {
		var err error
		if o, err = s.Seek(0, io.SeekCurrent); err != nil {
			o = 0
		}
	}
	return &Writer{
		w:                w,
		f:                f,
		s:                s,
		baseOffset:       o,
		lastRecordOffset: -1,
	}
}

// fillHeader fills in the header for the pending chunk.
func (w *Writer) fillHeader(last bool) {
	if w.i+headerSize > w.j || w.j > blockSize {
		panic("pebble/record: bad writer state")
	}
	if last {
		if w.first {
			w.buf[w.i+6] = fullChunkType
		} else {
			w.buf[w.i+6] = lastChunkType
		}
	} else {
		if w.first {
			w.buf[w.i+6] = firstChunkType
		} else {
			w.buf[w.i+6] = middleChunkType
		}
	}
	binary.LittleEndian.PutUint32(w.buf[w.i+0:w.i+4], crc.New(w.buf[w.i+6:w.j]).Value())
	binary.LittleEndian.PutUint16(w.buf[w.i+4:w.i+6], uint16(w.j-w.i-headerSize))
}

// writeBlock writes the buffered block to the underlying writer, and reserves
// space for the next chunk's header.
func (w *Writer) writeBlock() {
	_, w.err = w.w.Write(w.buf[w.written:])
	w.i = 0
	w.j = headerSize
	w.written = 0
	w.blockNumber++
}

// writePending finishes the current record and writes the buffer to the
// underlying writer.
func (w *Writer) writePending() {
	if w.err != nil {
		return
	}
	if w.pending {
		w.fillHeader(true)
		w.pending = false
	}
	_, w.err = w.w.Write(w.buf[w.written:w.j])
	w.written = w.j
}

// Close finishes the current record and closes the writer.
func (w *Writer) Close() error {
	w.writePending()
	if w.err != nil {
		return w.err
	}
	w.err = errors.New("pebble/record: closed Writer")
	return nil
}

// Flush finishes the current record, writes to the underlying writer, and
// flushes it if that writer implements interface{ Flush() error }.
func (w *Writer) Flush() error {
	// TODO(peter): Add locking to Writer. Writing the pending block requires
	// synchronization with Write/Finish, but calling Flush on the underlying
	// file does not.
	w.writePending()
	if w.err != nil {
		return w.err
	}
	if w.f != nil {
		w.err = w.f.Flush()
		return w.err
	}
	return nil
}

// Sync finishes the current record, writes to the underlying writer, and
// flushes and syncs it if the writer implements interface { Sync() error }.
func (w *Writer) Sync() error {
	if err := w.Flush(); err != nil {
		return err
	}
	if w.s != nil {
		w.err = w.s.Sync()
		return w.err
	}
	return nil
}

// Write writes the given data to the current record, starting a new record if
// one isn't currently active.
func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}

	if !w.pending {
		w.i = w.j
		w.j = w.j + headerSize
		// Check if there is room in the block for the header.
		if w.j > blockSize {
			// Fill in the rest of the block with zeroes.
			for k := w.i; k < blockSize; k++ {
				w.buf[k] = 0
			}
			w.writeBlock()
			if w.err != nil {
				return 0, w.err
			}
		}
		w.lastRecordOffset = w.baseOffset + w.blockNumber*blockSize + int64(w.i)
		w.first = true
		w.pending = true
	}

	n0 := len(p)
	for len(p) > 0 {
		// Write a block, if it is full.
		if w.j == blockSize {
			w.fillHeader(false)
			w.writeBlock()
			if w.err != nil {
				return 0, w.err
			}
			w.first = false
		}
		// Copy bytes into the buffer.
		n := copy(w.buf[w.j:], p)
		w.j += n
		p = p[n:]
	}
	return n0, nil
}

// Finish finishes the current record and writes to the underlying writer.
func (w *Writer) Finish() error {
	if w.pending {
		w.fillHeader(true)
		w.pending = false
	}
	return w.err
}

// LastRecordOffset returns the offset in the underlying io.Writer of the last
// record so far - the one created by the most recent Next call. It is the
// offset of the first chunk header, suitable to pass to Reader.SeekRecord.
//
// If that io.Writer also implements io.Seeker, the return value is an absolute
// offset, in the sense of io.SeekStart, regardless of whether the io.Writer
// was initially at the zero position when passed to NewWriter. Otherwise, the
// return value is a relative offset, being the number of bytes written between
// the NewWriter call and any records written prior to the last record.
//
// If there is no last record, i.e. nothing was written, LastRecordOffset will
// return ErrNoLastRecord.
func (w *Writer) LastRecordOffset() (int64, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.lastRecordOffset < 0 {
		return 0, ErrNoLastRecord
	}
	return w.lastRecordOffset, nil
}
