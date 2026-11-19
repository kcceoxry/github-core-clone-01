package gitproto_test

import (
	"bytes"
	"testing"

	"forge/internal/gitproto"
)

func TestPktLineRoundTrip(t *testing.T) {
	items := []any{"hello\n", "flush", "world\n", "delim", "response-end", "flush"}
	encoded, err := gitproto.EncodePktLines(items)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	packets, err := gitproto.DecodePktLines(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(packets) != 6 {
		t.Fatalf("got %d packets, want 6", len(packets))
	}
	if string(packets[0].Payload) != "hello\n" || packets[0].Kind != gitproto.PacketData {
		t.Fatalf("packet 0 = %v %q, want data hello", packets[0].Kind, packets[0].Payload)
	}
	if packets[1].Kind != gitproto.PacketFlush {
		t.Fatalf("packet 1 kind = %v, want flush", packets[1].Kind)
	}
	if string(packets[2].Payload) != "world\n" {
		t.Fatalf("packet 2 = %q, want world", packets[2].Payload)
	}
	if packets[3].Kind != gitproto.PacketDelim {
		t.Fatalf("packet 3 kind = %v, want delim", packets[3].Kind)
	}
	if packets[4].Kind != gitproto.PacketResponseEnd {
		t.Fatalf("packet 4 kind = %v, want response-end", packets[4].Kind)
	}
	if packets[5].Kind != gitproto.PacketFlush {
		t.Fatalf("packet 5 kind = %v, want flush", packets[5].Kind)
	}
}

func TestPktLineHeaderIsLowercaseHex(t *testing.T) {
	encoded, err := gitproto.EncodePktLineString("abc\n")
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(encoded[:4]) != "0008" {
		t.Fatalf("header = %q, want 0008", encoded[:4])
	}
}

func TestPktLineMalformedHeader(t *testing.T) {
	if _, err := gitproto.DecodePktLines([]byte("zzzznot-a-pkt-line")); err == nil {
		t.Fatalf("expected an error for a malformed pkt-line, got none")
	}
}

func TestPktLineTruncated(t *testing.T) {
	if _, err := gitproto.DecodePktLines([]byte("0010ab")); err == nil {
		t.Fatalf("expected an error for a truncated pkt-line, got none")
	}
}

func TestPktLineTooLong(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), gitproto.MaxPktPayload+1)
	if _, err := gitproto.EncodePktLine(payload); err == nil {
		t.Fatalf("expected an error for an oversized payload, got none")
	}
}

func TestSidebandChunksRespectMax(t *testing.T) {
	pack := bytes.Repeat([]byte("p"), 200000)
	chunks, err := gitproto.SidebandChunks(pack)
	if err != nil {
		t.Fatalf("sideband: %v", err)
	}
	packets, err := gitproto.DecodePktLines(chunks)
	if err != nil {
		t.Fatalf("decode sideband: %v", err)
	}
	var reassembled []byte
	for _, packet := range packets {
		if packet.Kind != gitproto.PacketData {
			t.Fatalf("sideband packet kind = %v, want data", packet.Kind)
		}
		if len(packet.Payload) > gitproto.MaxPktLine-4 {
			t.Fatalf("sideband pkt-line payload is %d bytes, over the limit", len(packet.Payload))
		}
		if packet.Payload[0] != 0x01 {
			t.Fatalf("sideband band = %d, want 1", packet.Payload[0])
		}
		reassembled = append(reassembled, packet.Payload[1:]...)
	}
	if !bytes.Equal(reassembled, pack) {
		t.Fatalf("sideband round-trip reassembled %d bytes, want %d", len(reassembled), len(pack))
	}
}
