// Package pack reads and writes git packfiles.
//
// A packfile is a flat file of zlib-compressed objects: a 12 byte header
// ("PACK", version, object count), one entry per object, and a 20 byte SHA-1
// trailer over everything before it. Each entry starts with a type and size
// varint, then the zlib stream, plus a base reference for delta entries.
// Deltas are resolved recursively, so an entry may be a delta of a delta,
// and a base may live in the same pack, in an earlier pack, or in the
// caller's store (a thin pack).
//
// The writer in this package writes packs without deltas. That is valid git:
// a pack with no deltas is only larger, never wrong. Delta writing is a
// later optimisation, not a correctness requirement.
//
// Zlib streams are pooled: a fresh flate compressor allocates on the order
// of a megabyte of tables per writer, which dominated pack builds (2.4 MB
// allocated per 3-object pack in profiling). Pooled writers and readers are
// reset before every use and hold no reference to a caller's buffer after
// they are returned.
package pack

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"sync"

	"forge/internal/store"
)

// zlibWriterPool reuses flate compressors across pack writes. A writer is
// Reset to the caller's buffer before use and Reset to io.Discard before
// it goes back, so a pooled writer never keeps a caller's buffer alive.
var zlibWriterPool = sync.Pool{
	New: func() any {
		w, err := zlib.NewWriterLevel(io.Discard, zlib.DefaultCompression)
		if err != nil {
			panic(fmt.Sprintf("pack: cannot create a zlib writer: %v", err))
		}
		return w
	},
}

// resettableZlibReader is the interface a pooled zlib reader must meet:
// the concrete type NewReader returns is unexported, but it carries
// Reset, so an interface assertion recovers it for pooling.
type resettableZlibReader interface {
	io.Reader
	Reset(io.Reader, []byte) error
}

// zlibReaderPool reuses flate decompressors across pack parses. A pooled
// reader is created over the smallest valid zlib stream (an empty deflate
// block), because NewReader reads the header at once and fails on empty
// input; every use resets it to the caller's slice.
var zlibReaderPool = sync.Pool{
	New: func() any {
		r, err := zlib.NewReader(bytes.NewReader([]byte{0x78, 0x9c, 0x03, 0x00, 0x00, 0x00, 0x00, 0x01}))
		if err != nil {
			panic(fmt.Sprintf("pack: cannot create a zlib reader: %v", err))
		}
		zr, ok := r.(resettableZlibReader)
		if !ok {
			panic("pack: the zlib reader has no Reset method")
		}
		return zr
	},
}

// MaxDeltaDepth caps how deep a delta chain may recurse. A chain deeper
// than this is either corrupt or hostile, never a real pack.
const MaxDeltaDepth = 50

// MaxObjectSize caps the decompressed size of a single object the parser
// will accept. It guards against a corrupt header claiming gigabytes.
const MaxObjectSize = 1 << 30

// entryKind says how an entry's bytes relate to its base object.
type entryKind int

const (
	kindPlain entryKind = iota
	kindOfsDelta
	kindRefDelta
)

// rawEntry is one entry as read from the pack, before delta resolution.
type rawEntry struct {
	offset     int // byte offset of this entry's header in the pack
	typ        store.ObjectType
	size       int    // declared decompressed size
	data       []byte // inflated bytes: content, or delta data for a delta
	kind       entryKind
	baseOffset int       // ofs-delta: absolute pack offset of the base entry
	baseOID    store.OID // ref-delta: identity of the base object
}

// Parse reads a whole packfile and returns its objects, resolved, in pack
// order. Thin-pack bases that are not in the pack are looked up through
// resolveBase, which may be nil when no external base is allowed. Every
// failure is an error, never a panic and never a half-resolved result.
func Parse(data []byte, resolveBase func(store.OID) (*store.Object, bool)) ([]*store.Object, error) {
	if len(data) < 12+20 {
		return nil, fmt.Errorf("pack: packfile is %d bytes, shorter than a header plus trailer", len(data))
	}
	if string(data[0:4]) != "PACK" {
		return nil, fmt.Errorf("pack: packfile does not start with PACK")
	}
	version := binary.BigEndian.Uint32(data[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack: unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(data[8:12]))

	trailerStart := len(data) - 20
	trailer := data[trailerStart:]
	sum := sha1.Sum(data[:trailerStart])
	if !bytes.Equal(trailer, sum[:]) {
		return nil, fmt.Errorf("pack: trailer checksum mismatch")
	}

	p := &parser{
		data:      data,
		byOffset:  make(map[int]int, count),
		resolved:  make(map[int]*store.Object, count),
		byOID:     make(map[store.OID]*store.Object, count),
		resolving: make(map[int]bool),
		external:  resolveBase,
	}
	offset := 12
	for i := 0; i < count; i++ {
		entry, next, err := p.parseEntry(offset, trailerStart)
		if err != nil {
			return nil, err
		}
		p.byOffset[entry.offset] = len(p.entries)
		p.entries = append(p.entries, entry)
		offset = next
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack: %d stray bytes before the trailer", trailerStart-offset)
	}

	out := make([]*store.Object, 0, len(p.entries))
	for i := range p.entries {
		obj, err := p.resolveEntry(i, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, obj)
	}
	return out, nil
}

// parser holds the state of one Parse call.
type parser struct {
	data      []byte
	entries   []*rawEntry
	byOffset  map[int]int // pack offset -> entry index
	resolved  map[int]*store.Object
	byOID     map[store.OID]*store.Object
	resolving map[int]bool
	external  func(store.OID) (*store.Object, bool)
}

// parseEntry reads one entry header and its zlib stream at offset. It
// returns the entry and the offset just past its zlib stream.
func (p *parser) parseEntry(offset, trailerStart int) (*rawEntry, int, error) {
	entryOffset := offset
	if offset >= trailerStart {
		return nil, 0, fmt.Errorf("pack: entry at offset %d starts at or past the trailer", offset)
	}
	typ, size, next, err := readTypeSize(p.data, offset)
	if err != nil {
		return nil, 0, err
	}
	offset = next
	if size < 0 || size > MaxObjectSize {
		return nil, 0, fmt.Errorf("pack: entry at offset %d declares impossible size %d", entryOffset, size)
	}
	entry := &rawEntry{offset: entryOffset, typ: typ, size: size}

	switch typ {
	case store.TypeCommit, store.TypeTree, store.TypeBlob, store.TypeTag:
		entry.kind = kindPlain
	case 6: // OBJ_OFS_DELTA
		entry.kind = kindOfsDelta
		base, n, err := readOfsBase(p.data, offset)
		if err != nil {
			return nil, 0, err
		}
		entry.baseOffset = entryOffset - base
		if entry.baseOffset < 12 || entry.baseOffset >= entryOffset {
			return nil, 0, fmt.Errorf("pack: ofs-delta at offset %d names base offset %d outside the pack before it", entryOffset, entry.baseOffset)
		}
		offset = n
	case 7: // OBJ_REF_DELTA
		entry.kind = kindRefDelta
		if offset+20 > trailerStart {
			return nil, 0, fmt.Errorf("pack: ref-delta at offset %d has a truncated base identity", entryOffset)
		}
		copy(entry.baseOID[:], p.data[offset:offset+20])
		offset += 20
	default:
		return nil, 0, fmt.Errorf("pack: entry at offset %d has unknown type %d", entryOffset, uint8(typ))
	}

	data, nextOffset, err := inflateAt(p.data, offset, trailerStart)
	if err != nil {
		return nil, 0, err
	}
	if len(data) != size {
		return nil, 0, fmt.Errorf("pack: entry at offset %d declares %d bytes but inflated to %d", entryOffset, size, len(data))
	}
	entry.data = data
	return entry, nextOffset, nil
}

// resolveEntry returns the resolved object for entry i, applying deltas
// recursively. Results are memoised, and a cycle or a chain deeper than
// MaxDeltaDepth is an error.
func (p *parser) resolveEntry(i int, depth int) (*store.Object, error) {
	if obj, ok := p.resolved[i]; ok {
		return obj, nil
	}
	if depth > MaxDeltaDepth {
		return nil, fmt.Errorf("pack: delta chain deeper than %d at entry %d", MaxDeltaDepth, i)
	}
	if p.resolving[i] {
		return nil, fmt.Errorf("pack: delta cycle at entry %d", i)
	}
	entry := p.entries[i]
	p.resolving[i] = true
	defer delete(p.resolving, i)

	var obj *store.Object
	switch entry.kind {
	case kindPlain:
		obj = store.NewObject(entry.typ, entry.data)
	case kindOfsDelta:
		baseIdx, ok := p.byOffset[entry.baseOffset]
		if !ok {
			return nil, fmt.Errorf("pack: ofs-delta at offset %d names base offset %d, which is not an entry", entry.offset, entry.baseOffset)
		}
		base, err := p.resolveEntry(baseIdx, depth+1)
		if err != nil {
			return nil, err
		}
		data, err := ApplyDelta(base.Data, entry.data)
		if err != nil {
			return nil, fmt.Errorf("pack: ofs-delta at offset %d: %w", entry.offset, err)
		}
		obj = store.NewObject(base.Type, data)
	case kindRefDelta:
		base, err := p.resolveBaseOID(entry.baseOID, depth+1)
		if err != nil {
			return nil, fmt.Errorf("pack: ref-delta at offset %d: %w", entry.offset, err)
		}
		data, err := ApplyDelta(base.Data, entry.data)
		if err != nil {
			return nil, fmt.Errorf("pack: ref-delta at offset %d: %w", entry.offset, err)
		}
		obj = store.NewObject(base.Type, data)
	}

	p.resolved[i] = obj
	p.byOID[obj.OID()] = obj
	return obj, nil
}

// resolveBaseOID finds the base object for a ref-delta: first among objects
// already resolved from this pack, then through the caller's lookup, then by
// resolving the pack's own unresolved entries one by one until one of them
// turns out to be the base.
func (p *parser) resolveBaseOID(oid store.OID, depth int) (*store.Object, error) {
	if obj, ok := p.byOID[oid]; ok {
		return obj, nil
	}
	if p.external != nil {
		if obj, ok := p.external(oid); ok {
			return obj, nil
		}
	}
	for i := range p.entries {
		if _, done := p.resolved[i]; done {
			continue
		}
		obj, err := p.resolveEntry(i, depth)
		if err != nil {
			continue
		}
		if obj.OID() == oid {
			return obj, nil
		}
	}
	return nil, fmt.Errorf("pack: ref-delta base %s is not in the pack or the store", oid.Hex())
}

// readTypeSize reads a pack entry header: the first byte carries the type in
// bits 4 to 6 and the low 4 bits of the size, and each following byte carries
// 7 more size bits while its top bit is set. It returns the type, the size
// in bytes after inflation, and the offset just past the header.
func readTypeSize(data []byte, offset int) (store.ObjectType, int, int, error) {
	if offset >= len(data) {
		return 0, 0, 0, fmt.Errorf("pack: entry at offset %d has no size header", offset)
	}
	b := data[offset]
	offset++
	typ := store.ObjectType((b >> 4) & 0x07)
	size := int(b & 0x0f)
	shift := 4
	for b&0x80 != 0 {
		if offset >= len(data) {
			return 0, 0, 0, fmt.Errorf("pack: entry size varint is truncated")
		}
		b = data[offset]
		offset++
		size |= int(b&0x7f) << shift
		shift += 7
		if shift > 63 {
			return 0, 0, 0, fmt.Errorf("pack: entry size varint is too long")
		}
	}
	return typ, size, offset, nil
}

// readOfsBase reads an OFS_DELTA base offset: a varint where each byte after
// the first adds one before shifting, which is the plus-one bias git uses so
// no offset byte is ever wasted on a zero. The result is the distance to
// walk backwards from the delta entry to its base.
func readOfsBase(data []byte, offset int) (int, int, error) {
	if offset >= len(data) {
		return 0, 0, fmt.Errorf("pack: ofs-delta has no base offset")
	}
	b := data[offset]
	offset++
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if offset >= len(data) {
			return 0, 0, fmt.Errorf("pack: ofs-delta base offset is truncated")
		}
		b = data[offset]
		offset++
		value = ((value + 1) << 7) | int(b&0x7f)
	}
	return value, offset, nil
}

// inflateAt inflates one zlib stream starting at offset and returns its
// bytes and the offset just past the stream. The offset is found by reading
// through a bytes.Reader, which the zlib reader consumes byte by byte with
// no read-ahead, so the position after the stream is exact. The zlib
// reader comes from a pool and is reset to the caller's slice each time.
func inflateAt(data []byte, offset, limit int) ([]byte, int, error) {
	if offset >= limit {
		return nil, 0, fmt.Errorf("pack: zlib stream at offset %d starts at or past the trailer", offset)
	}
	br := bytes.NewReader(data[offset:limit])
	zr := zlibReaderPool.Get().(resettableZlibReader)
	defer zlibReaderPool.Put(zr)
	if err := zr.Reset(br, nil); err != nil {
		return nil, 0, fmt.Errorf("pack: zlib stream at offset %d did not open: %w", offset, err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, fmt.Errorf("pack: zlib stream at offset %d did not inflate: %w", offset, err)
	}
	pos, _ := br.Seek(0, io.SeekCurrent)
	return out, offset + int(pos), nil
}

// Write builds a packfile holding the given objects with no deltas: the
// header, one zlib-compressed entry per object, and the SHA-1 trailer. The
// output is deterministic: the same objects in the same order always produce
// the same bytes.
func Write(objs []*store.Object) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("PACK")
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], 2)
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(objs)))
	buf.Write(hdr[:])

	for _, obj := range objs {
		if obj == nil {
			return nil, fmt.Errorf("pack: cannot write a nil object")
		}
		writeTypeSize(&buf, obj.Type, len(obj.Data))
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(&buf)
		if _, err := zw.Write(obj.Data); err != nil {
			zw.Reset(io.Discard)
			zlibWriterPool.Put(zw)
			return nil, fmt.Errorf("pack: compressing object %s: %w", obj.ID(), err)
		}
		if err := zw.Close(); err != nil {
			zw.Reset(io.Discard)
			zlibWriterPool.Put(zw)
			return nil, fmt.Errorf("pack: closing zlib stream for object %s: %w", obj.ID(), err)
		}
		zw.Reset(io.Discard)
		zlibWriterPool.Put(zw)
	}

	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}

// writeTypeSize writes a pack entry header: the type in bits 4 to 6 of the
// first byte, the low 4 bits of the size in the same byte, then 7 size bits
// per following byte while the size still has bits left.
func writeTypeSize(buf *bytes.Buffer, typ store.ObjectType, size int) {
	b := byte(typ<<4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		buf.WriteByte(b | 0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	buf.WriteByte(b)
}
