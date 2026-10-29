package nativewire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestDevinEnvelopeLengthBound(t *testing.T) {
	for _, n := range []int{0, devinMaxFrame, devinMaxFrame + 1} {
		p := make([]byte, n)
		framed := devinEnvelope(0, p)
		if n > devinMaxFrame {
			if framed != nil {
				t.Fatal("oversized envelope accepted")
			}
			continue
		}
		if len(framed) != n+5 || int64(binary.BigEndian.Uint32(framed[1:5])) != int64(n) {
			t.Fatal("incorrect frame length")
		}
		_, decoded, err := devinReadFrame(bytes.NewReader(framed))
		if err != nil || len(decoded) != n {
			t.Fatalf("boundary frame: len=%d err=%v", len(decoded), err)
		}
	}
}
func TestDevinProtobufFieldNumberBounds(t *testing.T) {
	for _, encode := range []struct {
		name string
		fn   func(int) []byte
	}{
		{"bytes", func(n int) []byte { return devinBytes(nil, n, []byte("x")) }},
		{"varint", func(n int) []byte { return devinInt(nil, n, 1) }},
		{"fixed64", func(n int) []byte { return devinDouble(nil, n, 1) }},
	} {
		t.Run(encode.name, func(t *testing.T) {
			for _, n := range []int{-1, 0, 1 << 29, math.MaxInt} {
				if encode.fn(n) != nil {
					t.Fatalf("accepted invalid field %d", n)
				}
			}
			for _, n := range []int{1, 1<<29 - 1} {
				fs, err := devinFields(encode.fn(n))
				if err != nil || len(fs) != 1 || fs[0].num != n {
					t.Fatalf("field=%d fields=%v err=%v", n, fs, err)
				}
			}
		})
	}
}
func TestDevinProtobufLengthOverflow(t *testing.T) {
	for _, n := range []uint64{1, 1 << 63, math.MaxUint64} {
		wire := binary.AppendUvarint([]byte{0x0a}, n)
		if _, err := devinFields(wire); err == nil {
			t.Fatalf("accepted length %d with no payload", n)
		}
	}
}
func TestDevinUsageStatusBounds(t *testing.T) {
	for _, n := range []uint64{0, 200, 399, 400, 401, 429, 500, 599, 600, 1 << 32, 1 << 63, math.MaxUint64} {
		var out devinOutput
		err := out.usageField(devinInt(nil, 6, n))
		var failure *devinFailure
		if !errors.As(err, &failure) {
			t.Fatalf("status %d did not fail safely: %v", n, err)
		}
		if n >= 400 && n <= 599 {
			if failure.status != int(n) || failure.code != "upstream_status" {
				t.Fatalf("valid status %d: %+v", n, failure)
			}
		} else {
			if failure.status != 502 || failure.code != "invalid_upstream_response" || failure.message != "invalid upstream error status" {
				t.Fatalf("unsanitized status %d: %+v", n, failure)
			}
		}
	}
}
func TestDevinDimensionFixed32Bounds(t *testing.T) {
	for _, tc := range []struct {
		bits  uint32
		valid bool
	}{
		{0, true}, {math.Float32bits(42), true}, {math.Float32bits(1 << 40), true},
		{math.MaxUint32, false}, {math.Float32bits(-1), false}, {math.Float32bits(1 << 41), false}, {math.Float32bits(0.5), false}, {0x7f800000, false},
	} {
		value := binary.LittleEndian.AppendUint32([]byte{0x15}, tc.bits)
		item := devinString(devinBytes(nil, 4, value), 5, "input_tokens")
		group := devinBytes(devinString(nil, 1, "Token Usage"), 2, item)
		var out devinOutput
		err := out.dimension(group)
		if (err == nil) != tc.valid {
			t.Fatalf("bits=%08x valid=%t err=%v", tc.bits, tc.valid, err)
		}
	}
}
