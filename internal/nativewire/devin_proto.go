/*
Protocol field mapping adapted from CLIProxyAPI d946c4f,
internal/runtime/executor/helps/devin_wire.go.
Source: https://github.com/router-for-me/CLIProxyAPI/tree/d946c4f
No client impersonation or prompt obfuscation is ported; device identity generation
is independently implemented for Ferry in the native auth driver.

# MIT License

Copyright (c) 2025-2005.9 Luis Pater
Copyright (c) 2025.9-present Router-For.ME

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/
package nativewire

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const devinMaxFrame = 16 << 20
const devinMaxDecoded = 64 << 20
const devinMaxTotal = 64 << 20

// A small bounded protobuf wire reader avoids importing the upstream SDK.
type devinField struct {
	num int
	typ int
	n   uint64
	b   []byte
}

func devinFields(b []byte) ([]devinField, error) {
	var out []devinField
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 || tag>>3 == 0 || tag>>3 > 1<<29-1 {
			return nil, errors.New("devin: invalid protobuf tag")
		}
		b = b[n:]
		f := devinField{num: int(tag >> 3), typ: int(tag & 7)}
		switch f.typ {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, io.ErrUnexpectedEOF
			}
			f.n = v
			b = b[n:]
		case 1:
			if len(b) < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			f.n = binary.LittleEndian.Uint64(b)
			b = b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, io.ErrUnexpectedEOF
			}
			b = b[n:]
			if l > uint64(len(b)) {
				return nil, io.ErrUnexpectedEOF
			}
			f.b = b[:l]
			b = b[l:]
		case 5:
			if len(b) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			f.n = uint64(binary.LittleEndian.Uint32(b))
			b = b[4:]
		default:
			return nil, errors.New("devin: unsupported protobuf wire type")
		}
		out = append(out, f)
		if len(out) > 65536 {
			return nil, errors.New("devin: too many protobuf fields")
		}
	}
	return out, nil
}
func devinBytes(dst []byte, n int, v []byte) []byte {
	if n < 1 || n > 1<<29-1 {
		return nil
	}
	dst = binary.AppendUvarint(dst, uint64(n)<<3|2)
	dst = binary.AppendUvarint(dst, uint64(len(v)))
	return append(dst, v...)
}
func devinString(dst []byte, n int, v string) []byte { return devinBytes(dst, n, []byte(v)) }
func devinInt(dst []byte, n int, v uint64) []byte {
	if n < 1 || n > 1<<29-1 {
		return nil
	}
	dst = binary.AppendUvarint(dst, uint64(n)<<3)
	return binary.AppendUvarint(dst, v)
}
func devinDouble(dst []byte, n int, v uint64) []byte {
	if n < 1 || n > 1<<29-1 {
		return nil
	}
	dst = binary.AppendUvarint(dst, uint64(n)<<3|1)
	return binary.LittleEndian.AppendUint64(dst, v)
}

// devinEnvelope returns nil for oversized payloads. Production callers already
// enforce this limit before framing; keep the narrowing conversion safe here too.
func devinEnvelope(flag byte, p []byte) []byte {
	size := len(p)
	if size < 0 || size > devinMaxFrame {
		return nil
	}
	b := make([]byte, 5, 5+len(p))
	b[0] = flag
	binary.BigEndian.PutUint32(b[1:], uint32(size))
	return append(b, p...)
}
func devinReadFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if h[0]&^byte(3) != 0 {
		return 0, nil, errors.New("devin: invalid Connect flags")
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > devinMaxFrame {
		return 0, nil, errors.New("devin: Connect frame exceeds limit")
	}
	b := make([]byte, int(n))
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	if h[0]&1 != 0 {
		z, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return 0, nil, fmt.Errorf("devin: gzip: %w", err)
		}
		b, err = io.ReadAll(io.LimitReader(z, devinMaxDecoded+1))
		z.Close()
		if err != nil {
			return 0, nil, err
		}
		if len(b) > devinMaxDecoded {
			return 0, nil, errors.New("devin: decompressed frame exceeds limit")
		}
	}
	return h[0], b, nil
}
