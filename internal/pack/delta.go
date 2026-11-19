package pack

import "fmt"

// ApplyDelta rebuilds an object from a base object's bytes and a delta.
//
// A delta starts with two size varints, the base size and the target size,
// then a stream of instructions. Each instruction is one opcode byte: a set
// top bit means COPY, a clear top bit means INSERT, and a zero byte is
// reserved and rejected. COPY reads its offset and size from the bytes that
// follow, using only the bytes the opcode's flag bits ask for, little-endian;
// a COPY with no size bytes means 0x10000 bytes. INSERT copies the next
// 1 to 127 bytes of the delta itself into the output.
//
// The result must be exactly the declared target size, and the declared base
// size must match the base given, or the delta is corrupt.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		value := 0
		shift := 0
		for {
			if off >= len(delta) {
				return 0, fmt.Errorf("delta: size varint is truncated")
			}
			b := delta[off]
			off++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return value, nil
			}
			if shift > 63 {
				return 0, fmt.Errorf("delta: size varint is too long")
			}
		}
	}

	sourceSize, err := readSize()
	if err != nil {
		return nil, err
	}
	targetSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if sourceSize != len(base) {
		return nil, fmt.Errorf("delta: expects a %d-byte base, got %d", sourceSize, len(base))
	}
	if targetSize < 0 || targetSize > MaxObjectSize {
		return nil, fmt.Errorf("delta: declares impossible target size %d", targetSize)
	}

	out := make([]byte, 0, targetSize)
	for off < len(delta) {
		op := delta[off]
		off++
		switch {
		case op == 0x00:
			return nil, fmt.Errorf("delta: reserved instruction 0x00")
		case op&0x80 != 0:
			// COPY: bits 0 to 3 say which offset bytes follow, bits 4 to 6
			// say which size bytes follow, both little-endian.
			copyOffset := 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, fmt.Errorf("delta: copy offset is truncated")
					}
					copyOffset |= int(delta[off]) << (8 * bit)
					off++
				}
			}
			copySize := 0
			for bit := 0; bit < 3; bit++ {
				if op&(0x10<<bit) != 0 {
					if off >= len(delta) {
						return nil, fmt.Errorf("delta: copy size is truncated")
					}
					copySize |= int(delta[off]) << (8 * bit)
					off++
				}
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset < 0 || copySize < 0 || copyOffset+copySize > len(base) {
				return nil, fmt.Errorf("delta: copy of %d bytes at offset %d runs past the %d-byte base", copySize, copyOffset, len(base))
			}
			out = append(out, base[copyOffset:copyOffset+copySize]...)
		default:
			// INSERT: the low 7 bits are the literal byte count, 1 to 127.
			n := int(op & 0x7f)
			if off+n > len(delta) {
				return nil, fmt.Errorf("delta: insert of %d bytes is truncated", n)
			}
			out = append(out, delta[off:off+n]...)
			off += n
		}
	}
	if len(out) != targetSize {
		return nil, fmt.Errorf("delta: rebuilt %d bytes, expected %d", len(out), targetSize)
	}
	return out, nil
}
