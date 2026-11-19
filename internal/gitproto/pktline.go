// Package gitproto implements git smart HTTP: protocol v0 and v2, pkt-line
// framing, ls-refs, fetch with shallow and filter support, and receive-pack.
//
// The contract is the kit's git-client.ts: every capability it requires, every
// request line it sends, and every response shape it parses. The server is
// liberal where the client is strict: unknown request lines are ignored, and
// a malformed request is answered 400 without ever taking the server down.
package gitproto

import (
	"bytes"
	"fmt"
)

// MaxPktLine is git's LARGE_PACKET_MAX: the largest pkt-line including its
// 4-byte length header.
const MaxPktLine = 65520

// MaxPktPayload is the largest payload a pkt-line can carry.
const MaxPktPayload = MaxPktLine - 4

// MaxSidebandData is the largest chunk of pack data one sideband packet may
// carry: the pkt-line header plus the one band byte plus this many bytes.
const MaxSidebandData = 65500

// PacketKind is one decoded pkt-line.
type PacketKind int

const (
	// PacketData is a normal pkt-line carrying a payload.
	PacketData PacketKind = iota
	// PacketFlush is the 0000 flush packet.
	PacketFlush
	// PacketDelim is the 0001 delimiter packet (v2 sections).
	PacketDelim
	// PacketResponseEnd is the 0002 response-end packet (v2).
	PacketResponseEnd
)

// Packet is one decoded pkt-line.
type Packet struct {
	Kind    PacketKind
	Payload []byte // data packets only
}

// EncodePktLine encodes one data packet: a 4-hex-digit length (payload plus
// the 4 header bytes) followed by the payload.
func EncodePktLine(payload []byte) ([]byte, error) {
	if len(payload) > MaxPktPayload {
		return nil, fmt.Errorf("gitproto: pkt-line payload is %d bytes, over the %d-byte maximum", len(payload), MaxPktPayload)
	}
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(payload)+4))...)
	out = append(out, payload...)
	return out, nil
}

// EncodePktLineString encodes one data packet from a string payload.
func EncodePktLineString(payload string) ([]byte, error) {
	return EncodePktLine([]byte(payload))
}

// EncodePktLines encodes a sequence of data packets and control packets.
// The strings "flush", "delim" and "response-end" name the control packets.
func EncodePktLines(items []any) ([]byte, error) {
	var buf bytes.Buffer
	for _, item := range items {
		switch v := item.(type) {
		case string:
			switch v {
			case "flush":
				buf.WriteString("0000")
			case "delim":
				buf.WriteString("0001")
			case "response-end":
				buf.WriteString("0002")
			default:
				line, err := EncodePktLineString(v)
				if err != nil {
					return nil, err
				}
				buf.Write(line)
			}
		case []byte:
			line, err := EncodePktLine(v)
			if err != nil {
				return nil, err
			}
			buf.Write(line)
		default:
			return nil, fmt.Errorf("gitproto: cannot encode pkt-line item of type %T", item)
		}
	}
	return buf.Bytes(), nil
}

// DecodePktLines decodes a complete buffer into packets. A malformed header
// or a truncated packet is an error, never a partial result.
func DecodePktLines(data []byte) ([]Packet, error) {
	var packets []Packet
	offset := 0
	for offset < len(data) {
		if len(data)-offset < 4 {
			return nil, fmt.Errorf("gitproto: truncated pkt-line: %d trailing byte(s)", len(data)-offset)
		}
		header := string(data[offset : offset+4])
		length, err := parseHex4(header)
		if err != nil {
			return nil, fmt.Errorf("gitproto: invalid pkt-line length %q", header)
		}
		switch length {
		case 0:
			packets = append(packets, Packet{Kind: PacketFlush})
			offset += 4
		case 1:
			packets = append(packets, Packet{Kind: PacketDelim})
			offset += 4
		case 2:
			packets = append(packets, Packet{Kind: PacketResponseEnd})
			offset += 4
		case 3:
			return nil, fmt.Errorf("gitproto: invalid pkt-line length 0003")
		default:
			if length > MaxPktLine {
				return nil, fmt.Errorf("gitproto: pkt-line length %d exceeds %d", length, MaxPktLine)
			}
			if len(data)-offset < length {
				return nil, fmt.Errorf("gitproto: truncated pkt-line: %d announced, %d available", length, len(data)-offset)
			}
			payload := make([]byte, length-4)
			copy(payload, data[offset+4:offset+length])
			packets = append(packets, Packet{Kind: PacketData, Payload: payload})
			offset += length
		}
	}
	return packets, nil
}

// parseHex4 parses a 4-character hex length, the way a pkt-line header is
// written. Uppercase hex is accepted, as git writes lowercase but reads both.
func parseHex4(s string) (int, error) {
	if len(s) != 4 {
		return 0, fmt.Errorf("not 4 characters")
	}
	n := 0
	for i := 0; i < 4; i++ {
		c := s[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return 0, fmt.Errorf("not hex")
		}
		n = n*16 + v
	}
	return n, nil
}

// SidebandChunks splits pack bytes into sideband band-1 packets: each one is
// a pkt-line holding one band byte (0x01) plus up to MaxSidebandData bytes of
// pack data, so no pkt-line ever exceeds the 65520-byte limit. An empty pack
// produces no packets; callers always pass a real pack (at least the 32-byte
// header plus trailer of an empty pack).
func SidebandChunks(pack []byte) ([]byte, error) {
	var buf bytes.Buffer
	for offset := 0; offset < len(pack); {
		end := offset + MaxSidebandData
		if end > len(pack) {
			end = len(pack)
		}
		chunk := make([]byte, 0, end-offset+1)
		chunk = append(chunk, 0x01)
		chunk = append(chunk, pack[offset:end]...)
		line, err := EncodePktLine(chunk)
		if err != nil {
			return nil, err
		}
		buf.Write(line)
		offset = end
	}
	return buf.Bytes(), nil
}

// SplitPktLines splits a raw request body into its pkt-line packets and the
// trailing raw bytes after the first flush packet. Receive-pack bodies carry
// a pack after the command list's flush; the pack is the trailing bytes.
func SplitPktLines(data []byte) (packets []Packet, rest []byte, err error) {
	offset := 0
	for offset < len(data) {
		if len(data)-offset < 4 {
			return nil, nil, fmt.Errorf("gitproto: truncated pkt-line: %d trailing byte(s)", len(data)-offset)
		}
		header := string(data[offset : offset+4])
		length, perr := parseHex4(header)
		if perr != nil {
			return nil, nil, fmt.Errorf("gitproto: invalid pkt-line length %q", header)
		}
		switch length {
		case 0:
			packets = append(packets, Packet{Kind: PacketFlush})
			return packets, data[offset+4:], nil
		case 1:
			packets = append(packets, Packet{Kind: PacketDelim})
			offset += 4
		case 2:
			packets = append(packets, Packet{Kind: PacketResponseEnd})
			offset += 4
		case 3:
			return nil, nil, fmt.Errorf("gitproto: invalid pkt-line length 0003")
		default:
			if length > MaxPktLine {
				return nil, nil, fmt.Errorf("gitproto: pkt-line length %d exceeds %d", length, MaxPktLine)
			}
			if len(data)-offset < length {
				return nil, nil, fmt.Errorf("gitproto: truncated pkt-line: %d announced, %d available", length, len(data)-offset)
			}
			payload := make([]byte, length-4)
			copy(payload, data[offset+4:offset+length])
			packets = append(packets, Packet{Kind: PacketData, Payload: payload})
			offset += length
		}
	}
	return packets, nil, nil
}
