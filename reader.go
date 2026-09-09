/*
Copyright 2020 Google LLC

Use of this source code is governed by a BSD-style
license that can be found in the LICENSE file or at
https://developers.google.com/open-source/licenses/bsd
*/

package reftable

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"log"
)

// ByteBlockSource is an in-memory block source.
type ByteBlockSource struct {
	Source []byte
}

func (s *ByteBlockSource) Size() uint64 {
	return uint64(len(s.Source))
}
func (s *ByteBlockSource) ReadBlock(off uint64, sz int) ([]byte, error) {
	return s.Source[off : off+uint64(sz)], nil
}

func (s *ByteBlockSource) Close() error {
	return nil
}

// readerOffsets has metadata for each block type
type readerOffsets struct {
	// Present is true if the section is present in the file.
	Present bool

	// Offset is where to find the first block of this type.
	Offset uint64

	// Offset of the index, or 0 if not present
	IndexOffset uint64
}

// Reader allows reading from a reftable.
type Reader struct {
	header header
	footer footer

	// Version format for the table
	version int

	objectIDLen int
	hashSize    int

	name string
	src  BlockSource
	size uint64

	offsets map[byte]readerOffsets
}

func (r *Reader) HashID() HashID {
	return r.header.HashID
}

func (r *Reader) DebugData() string {
	return fmt.Sprintf("name %s, sz %d hash %d: 'r' %#v, 'o' %#v 'g' %#v", r.name, r.size, r.hashSize,
		r.offsets['r'],
		r.offsets['o'],
		r.offsets['g'],
	)
}

func (r *Reader) Close() {
	r.src.Close()
}

func (r *Reader) Name() string {
	return r.name
}

func (r *Reader) getBlock(off uint64, sz uint32) ([]byte, error) {
	if off >= r.size {
		return nil, nil
	}

	if off+uint64(sz) > r.size {
		sz = uint32(r.size - off)
	}

	return r.src.ReadBlock(off, int(sz))
}

// readHeader reads the header from the given input source.
func readHeader(r io.Reader, h *header, version int) error {
	buf := make([]byte, headerSize(version))
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	if version == 1 {
		buf = append(buf, "sha1"...)
	}

	return binary.Read(bytes.NewBuffer(buf), binary.BigEndian, h)
}

// NewReader creates a reader for a reftable file.
func NewReader(src BlockSource, name string) (*Reader, error) {
	srcSize := src.Size()
	if srcSize == 0 {
		return nil, fmt.Errorf("reftable: empty table %q", name)
	}
	if srcSize < uint64(headerSize(1)+footerSize(1)) {
		return nil, fmt.Errorf("reftable: file %q is %d bytes, too small for header+footer", name, srcSize)
	}

	headBlock, err := src.ReadBlock(0, headerSize(2)+1)

	if err != nil {
		return nil, err
	}
	if bytes.Compare(headBlock[:4], magic[:]) != 0 {
		return nil, fmt.Errorf("reftable: got magic %q, want %q", headBlock[:4], magic)
	}

	version := int(headBlock[4])
	if version != 1 && version != 2 {
		return nil, fmt.Errorf("reftable: unsupported version %d", version)
	}

	if srcSize < uint64(headerSize(version)+footerSize(version)) {
		return nil, fmt.Errorf("reftable: file %q is %d bytes, too small for v%d header+footer", name, srcSize, version)
	}

	r := &Reader{
		version: version,
		size:    srcSize - uint64(footerSize(version)),
		src:     src,
		name:    name,
	}

	footBlock, err := src.ReadBlock(r.size, footerSize(version))
	if err != nil {
		return nil, err
	}

	if 0 != bytes.Compare(headBlock[:headerSize(version)], footBlock[:headerSize(version)]) {
		return nil, fmt.Errorf("reftable: start header %q != tail header %q",
			headBlock[:headerSize(version)], footBlock[:headerSize(version)])
	}

	footBuf := bytes.NewBuffer(footBlock)
	if err := readHeader(footBuf, &r.header, version); err != nil {
		return nil, err
	}
	if err := binary.Read(footBuf, binary.BigEndian, &r.footer); err != nil {
		return nil, err
	}

	var gotCRC32 uint32
	if err := binary.Read(footBuf, binary.BigEndian, &gotCRC32); err != nil {
		return nil, err
	}

	// The hash ID is 4 raw bytes out of the file. Validate it before use:
	// this runs before the CRC is compared below, so an unknown value here
	// is reachable with no valid checksum at all.
	r.hashSize = r.header.HashID.Size()
	if r.hashSize == 0 {
		return nil, fmt.Errorf("%w: unknown hash id %q", fmtError, string(r.header.HashID[:]))
	}
	r.header.BlockSize &= (1 << 24) - 1

	if footBuf.Len() > 0 {
		return nil, fmt.Errorf("%w: trailing footer bytes: %d", fmtError, footBuf.Len())
	}

	r.objectIDLen = int(r.footer.ObjOffset & ((1 << 5) - 1))
	r.footer.ObjOffset >>= 5

	wantCRC32 := crc32.ChecksumIEEE(footBlock[:footerSize(version)-4])
	if gotCRC32 != wantCRC32 {
		return nil, fmt.Errorf("reftable: got CRC %x, want CRC %x", gotCRC32, wantCRC32)
	}

	if r.footer.ObjOffset > 0 && r.objectIDLen == 0 {
		return nil, fmt.Errorf("reftable: object index present but object_id_len is 0")
	}

	firstBlockTyp := headBlock[headerSize(version)]
	r.offsets = map[byte]readerOffsets{
		blockTypeRef: {
			Present:     firstBlockTyp == blockTypeRef,
			Offset:      0,
			IndexOffset: r.footer.RefIndexOffset,
		},
		blockTypeLog: {
			Present:     firstBlockTyp == blockTypeLog || r.footer.LogOffset > 0,
			Offset:      r.footer.LogOffset,
			IndexOffset: r.footer.LogIndexOffset,
		},
		blockTypeObj: {
			Present:     r.footer.ObjOffset > 0,
			Offset:      r.footer.ObjOffset,
			IndexOffset: r.footer.ObjIndexOffset,
		},
	}

	// In case of blocksize==0, should read the entire thing
	// into a ByteBlockSource?

	return r, nil
}

// tableIter iterates over a section in the file. It is a value type,
// which can be copied with copyFrom.
type tableIter struct {
	r        *Reader
	typ      byte
	blockOff uint64
	bi       blockIter
	finished bool
}

// nextInBlock advances the block iterator, or returns false we are
// past the last record.
func (i *tableIter) nextInBlock(rec record) (bool, error) {
	ok, err := i.bi.Next(rec)
	if ok {
		r, isRef := rec.(*RefRecord)
		if isRef {
			r.UpdateIndex += i.r.header.MinUpdateIndex
		}
	}
	if err != nil {
		err = fmt.Errorf("block %c, off %d: %w", i.typ, i.blockOff, err)
	}
	return ok, err
}

// Next implements the Iterator interface
func (i *tableIter) Next(rec record) (bool, error) {
	if rec.typ() != i.typ {
		log.Panicf("got %T want %c", rec, i.typ)
	}

	for {
		if i.finished {
			return false, nil
		}

		ok, err := i.nextInBlock(rec)
		if err != nil || ok {
			return ok, err
		}
		ok, err = i.nextBlock()
		if err != nil {
			return false, err
		}
		if !ok {
			return ok, err
		}
	}
}

// extractBlockSize returns the block size from the block header
func extractBlockSize(block []byte, off uint64, version int) (typ byte, size uint32, err error) {
	if off == 0 {
		if len(block) < headerSize(version) {
			return 0, 0, fmtError
		}
		block = block[headerSize(version):]
	}

	if len(block) < 4 || !isBlockType(block[0]) {
		return 0, 0, fmtError
	}

	return block[0], getU24(block[1:]), nil
}

// newBlockReader opens a block of the given type, starting at
// nextOff. It is not an error to read beyond the end of file, or
// specify an offset into a different type of block. If this happens,
// a nil blockReader is returned.
func (r *Reader) newBlockReader(nextOff uint64, wantTyp byte) (br *blockReader, err error) {
	if nextOff >= r.size {
		return
	}

	guessBlockSize := r.header.BlockSize
	if guessBlockSize == 0 {
		guessBlockSize = defaultBlockSize
	}
	block, err := r.getBlock(nextOff, guessBlockSize)
	if err != nil {
		return nil, err
	}

	blockTyp, blockSize, err := extractBlockSize(block, nextOff, r.version)
	if err != nil {
		return nil, err
	}

	if wantTyp != blockTypeAny && blockTyp != wantTyp {
		return nil, nil
	}

	if blockSize > guessBlockSize {
		block, err = r.getBlock(nextOff, blockSize)
		if err != nil {
			return nil, err
		}
	}

	var headerOff uint32
	if nextOff == 0 {
		headerOff = uint32(headerSize(r.version))
	}

	return newBlockReader(block, headerOff, r.header.BlockSize, r.hashSize)
}

// nextBlock moves to the next block, or returns false if there is none.
//
// We rely on newBlockReader's wantTyp check to detect the boundary
// between block types. If a section ends and the next byte at
// nextBlockOff is not a recognized block type byte, isBlockType
// rejects it; if it is recognized but does not match i.typ,
// newBlockReader returns (nil, nil) and iteration stops cleanly.
//
// TODO: there is still a low-probability false-positive case where a
// random data byte at nextBlockOff happens to match both isBlockType
// and the wanted type. The C version (table.c table_iter_next_block)
// avoids this by tracking explicit per-section end offsets at iter
// creation time and refusing to advance past them. Plumbing the
// section bound through tableIter would be a non-trivial change.
func (i *tableIter) nextBlock() (bool, error) {
	nextBlockOff := i.blockOff + uint64(i.bi.br.fullBlockSize)
	br, err := i.r.newBlockReader(nextBlockOff, i.typ)
	if err != nil {
		return false, fmt.Errorf("reftable: reading %c block at 0x%x: %w", i.typ, nextBlockOff, err)
	}
	if br == nil {
		i.finished = true
		return false, nil
	}
	br.start(&i.bi)
	i.blockOff = nextBlockOff
	return true, nil
}

// start returns an iterator positioned at the start of the given
// block type. If index is specified, it returns an iterator at the
// start of the top-level index for that block type.
func (r *Reader) start(typ byte, index bool) (*tableIter, error) {
	off := r.offsets[typ].Offset
	if index {
		off = r.offsets[typ].IndexOffset
		typ = blockTypeIndex
		if off == 0 {
			return nil, nil
		}
	}
	return r.tabIterAt(off, typ)
}

// tabIterAt returns a tableIter for the data at given offset.
func (r *Reader) tabIterAt(off uint64, wantTyp byte) (*tableIter, error) {
	br, err := r.newBlockReader(off, wantTyp)
	if err != nil || br == nil {
		return nil, err
	}

	ti := &tableIter{
		r:        r,
		typ:      br.getType(),
		blockOff: off,
	}
	br.start(&ti.bi)
	return ti, nil
}

// seekRecord returns an iterator pointed to just before the key specified
// by the record
func (r *Reader) seekRecord(rec record) (iterator, error) {
	if !r.offsets[rec.typ()].Present {
		return &emptyIterator{}, nil
	}
	tabIter, err := r.seek(rec)
	if tabIter == nil {
		return &emptyIterator{}, err
	}
	return tabIter, err
}

func (r *Reader) SeekRef(name string) (*Iterator, error) {
	ref := RefRecord{
		RefName: name,
	}
	impl, err := r.seekRecord(&ref)
	if err != nil {
		return nil, err
	}
	return &Iterator{impl}, nil
}

func (r *Reader) SeekLog(name string, updateIndex uint64) (*Iterator, error) {
	log := LogRecord{
		RefName:     name,
		UpdateIndex: updateIndex,
	}

	impl, err := r.seekRecord(&log)
	if err != nil {
		return nil, err
	}
	return &Iterator{impl}, nil
}

// seek seeks to the key specified by the record
func (r *Reader) seek(rec record) (*tableIter, error) {
	typ := rec.typ()
	if rec.key() == newRecord(rec.typ(), "").key() {
		return r.start(typ, false)
	}

	idx := r.offsets[typ].IndexOffset
	if idx > 0 {
		return r.seekIndexed(rec)
	}

	tabIter, err := r.start(rec.typ(), false)
	if err != nil {
		return nil, err
	}
	if tabIter == nil {
		// No block of this type at the recorded offset; seekRecord turns a
		// nil iterator into an empty one.
		return nil, nil
	}

	ok, err := r.seekLinear(tabIter, rec)
	if ok {
		return tabIter, nil
	}
	return nil, err
}

// seekIndexed seeks to the `want` record, using its index.
func (r *Reader) seekIndexed(want record) (*tableIter, error) {
	idxIter, err := r.start(want.typ(), true)
	if err != nil {
		return nil, err
	}

	wantIdx := &indexRecord{
		LastKey: want.key(),
	}

	// seekIndexed is only reached when IndexOffset is non-zero, so a nil
	// iterator here means that offset does not address an index block.
	if idxIter == nil {
		return nil, fmt.Errorf("%w: index offset %d does not address an index block",
			fmtError, r.offsets[want.typ()].IndexOffset)
	}

	ok, err := r.seekLinear(idxIter, wantIdx)
	if err != nil || !ok {
		return nil, err
	}

	// Every offset below is read out of the file, so each descent can be made
	// to point anywhere, including back at the block we came from. Bound the
	// walk: a real index tree has a handful of levels, since each one holds
	// strictly fewer blocks than the level beneath it.
	for depth := 0; ; depth++ {
		if depth > maxIndexDepth {
			return nil, fmt.Errorf("%w: index deeper than %d levels, probably cyclic",
				fmtError, maxIndexDepth)
		}

		var rec indexRecord
		ok, err := idxIter.Next(&rec)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}

		tabIter, err := r.tabIterAt(rec.Offset, blockTypeAny)
		if err != nil {
			return nil, err
		}
		if tabIter == nil {
			// tabIterAt returns (nil, nil) when the offset is past EOF or
			// otherwise unusable. The offset came from the file, so this is
			// malformed input, not an empty result.
			return nil, fmt.Errorf("%w: index entry points at offset %d, which is not a block",
				fmtError, rec.Offset)
		}

		if err := tabIter.bi.seek(want.key()); err != nil {
			return nil, err
		}

		if tabIter.typ == want.typ() {
			return tabIter, nil
		}

		if tabIter.typ != blockTypeIndex {
			return nil, fmt.Errorf("%w: index entry at offset %d has block type %c, want %c or %c",
				fmtError, rec.Offset, tabIter.typ, want.typ(), blockTypeIndex)
		}

		idxIter = tabIter
	}
}

// seekLinear iterates tabIter to just before the wanted record.
func (r *Reader) seekLinear(tabIter *tableIter, want record) (bool, error) {
	rec := newRecord(want.typ(), "")

	wantKey := want.key()
	// skip blocks
	var last tableIter
	for {
		last = *tabIter

		ok, err := tabIter.nextBlock()
		if err != nil {
			return false, err
		}

		if !ok {
			break
		}

		ok, err = tabIter.Next(rec)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("%w: block at offset %d yielded no records",
				fmtError, tabIter.blockOff)
		}
		if rec.key() > wantKey {
			break
		}
	}

	// within the block, skip the right key
	*tabIter = last
	var err error
	err = tabIter.bi.seek(wantKey)
	if err != nil {
		return false, err
	}

	return true, nil
}

func (r *Reader) MaxUpdateIndex() uint64 {
	return r.header.MaxUpdateIndex
}

func (r *Reader) MinUpdateIndex() uint64 {
	return r.header.MinUpdateIndex
}

// indexedTableRefIter iterates over a refs, returning refs pointing
// to a given object ID. The ref blocks to consider must be specified
// upfront.
type indexedTableRefIter struct {
	r   *Reader
	oid []byte

	// mutable

	// block offsets of remaining refblocks to look into
	offsets  []uint64
	cur      blockIter
	finished bool
}

func (i *indexedTableRefIter) nextBlock() error {
	if len(i.offsets) == 0 {
		i.finished = true
		return nil
	}
	nextOff := i.offsets[0]
	i.offsets = i.offsets[1:]

	br, err := i.r.newBlockReader(nextOff, blockTypeRef)
	if err != nil {
		return err
	}
	if br == nil {
		return fmt.Errorf("reftable: indexed block does not exist")
	}

	br.start(&i.cur)
	return nil
}

// Next implements the Iterator interface
func (i *indexedTableRefIter) Next(rec record) (bool, error) {
	ref := rec.(*RefRecord)
	for {
		ok, err := i.cur.Next(ref)
		if err != nil {
			return false, err
		}
		if !ok {
			if err := i.nextBlock(); err != nil {
				return false, err
			}
			if i.finished {
				return false, nil
			}
			// XXX test for this case
			continue
		}

		if bytes.Compare(ref.Value, i.oid) == 0 || bytes.Compare(ref.TargetValue, i.oid) == 0 {
			ref.UpdateIndex += i.r.header.MinUpdateIndex
			return true, nil
		}
	}
}

// RefsFor iterates over refs that point to `oid`.
func (r *Reader) RefsFor(oid []byte) (*Iterator, error) {
	if r.offsets[blockTypeObj].Present {
		return r.refsForIndexed(oid)
	}

	it, err := r.start(blockTypeRef, false)
	if err != nil {
		return nil, err
	}
	if it == nil {
		return &Iterator{&emptyIterator{}}, nil
	}
	return &Iterator{&filteringRefIterator{
		tab:         r,
		oid:         oid,
		doubleCheck: false,
		it:          it,
	}}, nil
}

func (r *Reader) refsForIndexed(oid []byte) (*Iterator, error) {
	// objectIDLen comes from the footer and can exceed the hash the caller
	// passed, e.g. a 20 byte SHA-1 against a table declaring 31.
	if r.objectIDLen > len(oid) {
		return nil, fmt.Errorf("%w: table declares object id length %d, got a %d byte id",
			fmtError, r.objectIDLen, len(oid))
	}
	want := &objRecord{HashPrefix: oid[:r.objectIDLen]}

	it, err := r.seek(want)
	if err != nil {
		return nil, err
	}
	if it == nil {
		return &Iterator{&emptyIterator{}}, nil
	}

	got := objRecord{}
	ok, err := it.Next(&got)
	if err != nil {
		return nil, err
	}
	if !ok || got.key() != want.key() {
		return &Iterator{&emptyIterator{}}, nil
	}

	tr := &indexedTableRefIter{
		r:       r,
		oid:     oid,
		offsets: got.Offsets,
	}
	if err := tr.nextBlock(); err != nil {
		return nil, err
	}
	return &Iterator{tr}, nil
}
