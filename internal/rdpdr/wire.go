package rdpdr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf16"
)

var errShortPacket = errors.New("rdpdr: packet too short")

type reader struct {
	buf []byte
	err error
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || len(r.buf) < n {
		r.err = errShortPacket
		r.buf = nil
		return nil
	}
	b := r.buf[:n]
	r.buf = r.buf[n:]
	return b
}

func (r *reader) u8() uint8 {
	b := r.take(1)
	if b == nil {
		return 0
	}
	return b[0]
}

func (r *reader) u16() uint16 {
	b := r.take(2)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(b)
}

func (r *reader) u32() uint32 {
	b := r.take(4)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

func (r *reader) u64() uint64 {
	b := r.take(8)
	if b == nil {
		return 0
	}
	return binary.LittleEndian.Uint64(b)
}

func (r *reader) skip(n int) { r.take(n) }

func (r *reader) utf16z(byteLen int) string {
	return decodeUTF16(r.take(byteLen))
}

type writer struct{ buf []byte }

func (w *writer) u8(v uint8) { w.buf = append(w.buf, v) }
func (w *writer) u16(v uint16) {
	w.buf = binary.LittleEndian.AppendUint16(w.buf, v)
}
func (w *writer) u32(v uint32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}
func (w *writer) u64(v uint64) {
	w.buf = binary.LittleEndian.AppendUint64(w.buf, v)
}
func (w *writer) bytes(b []byte) { w.buf = append(w.buf, b...) }
func (w *writer) zeros(n int)    { w.buf = append(w.buf, make([]byte, n)...) }

func encodeUTF16(s string, nullTerminated bool) []byte {
	units := utf16.Encode([]rune(s))
	if nullTerminated {
		units = append(units, 0)
	}
	out := make([]byte, 0, len(units)*2)
	for _, u := range units {
		out = binary.LittleEndian.AppendUint16(out, u)
	}
	return out
}

func decodeUTF16(b []byte) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u := binary.LittleEndian.Uint16(b[i:])
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return string(utf16.Decode(units))
}

const filetimeUnixEpoch = 116444736000000000

func filetime(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UnixNano()/100) + filetimeUnixEpoch
}

func fromFiletime(ft uint64) (time.Time, bool) {
	if ft == 0 || ft == ^uint64(0) || ft < filetimeUnixEpoch {
		return time.Time{}, false
	}
	return time.Unix(0, int64(ft-filetimeUnixEpoch)*100).UTC(), true
}

func header(component, packetID uint16) writer {
	w := writer{buf: make([]byte, 0, 64)}
	w.u16(component)
	w.u16(packetID)
	return w
}

type packetError struct {
	what string
	err  error
}

func (e *packetError) Error() string { return fmt.Sprintf("rdpdr: parsing %s: %v", e.what, e.err) }
func (e *packetError) Unwrap() error { return e.err }
