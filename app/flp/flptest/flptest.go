// Package flptest builds small, valid FL Studio project files for tests.
package flptest

import "encoding/binary"

func utf16z(s string) []byte {
	var out []byte
	for _, r := range s {
		out = binary.LittleEndian.AppendUint16(out, uint16(r))
	}
	return binary.LittleEndian.AppendUint16(out, 0)
}

func event(b []byte, id byte, value []byte) []byte {
	b = append(b, id)
	n := len(value)
	for {
		c := byte(n & 0x7F)
		n >>= 7
		if n != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if n == 0 {
			break
		}
	}
	return append(b, value...)
}

// Project returns the bytes of a project saved by FL Studio 25.1.6 with the
// given title (may be empty), one native plugin and the given sample paths.
// filler makes two otherwise equal projects differ.
func Project(title string, samples []string, filler string) []byte {
	var e []byte
	e = event(e, 199, append([]byte("25.1.6.4997"), 0))
	if title != "" {
		e = event(e, 194, utf16z(title))
	}
	e = event(e, 201, utf16z("Fruity Limiter"))
	e = event(e, 213, make([]byte, 52))
	for _, sample := range samples {
		e = event(e, 196, utf16z(sample))
	}
	if filler != "" {
		e = event(e, 210, []byte(filler)) // an event the reader skips
	}
	f := []byte("FLhd")
	f = binary.LittleEndian.AppendUint32(f, 6)
	f = binary.LittleEndian.AppendUint16(f, 0)  // format
	f = binary.LittleEndian.AppendUint16(f, 1)  // channels
	f = binary.LittleEndian.AppendUint16(f, 96) // ppq
	f = append(f, "FLdt"...)
	f = binary.LittleEndian.AppendUint32(f, uint32(len(e)))
	return append(f, e...)
}
