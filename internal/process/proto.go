package process

// A minimal protobuf wire codec. The Datadog process intake (Live Processes)
// speaks protobuf, not JSON, so (to keep the agent free of a protobuf library,
// cgo, and zstd) we emit and read the wire format by hand. We only ever encode
// (the request payloads) and decode one tiny response (ResCollector), so this is
// far smaller than a general protobuf implementation: an append-only writer plus
// a field-walker that skips everything it doesn't recognise.
//
// Wire types we use: varint=0, fixed64=1, length-delimited=2, fixed32=5. Field
// tags are (fieldNumber<<3 | wireType), themselves varint-encoded. The payloads
// are proto3 with implicit presence, so a zero/empty field is indistinguishable
// from an absent one: every writer below skips zeros, which both shrinks the
// payload and matches how the stock Agent leaves unset fields off the wire.

import "math"

// pbuf is an append-only protobuf writer.
type pbuf struct{ b []byte }

// uvarint appends v as a base-128 varint.
func (w *pbuf) uvarint(v uint64) {
	for v >= 0x80 {
		w.b = append(w.b, byte(v)|0x80)
		v >>= 7
	}
	w.b = append(w.b, byte(v))
}

// tag appends a field tag.
func (w *pbuf) tag(field, wire int) { w.uvarint(uint64(field)<<3 | uint64(wire)) }

// uint writes a varint field, omitting it when zero. Used for every integer and
// enum field. Our integer fields are all non-negative so plain varint is correct.
func (w *pbuf) uint(field int, v uint64) {
	if v == 0 {
		return
	}
	w.tag(field, 0)
	w.uvarint(v)
}

// str writes a length-delimited string field, omitting it when empty.
func (w *pbuf) str(field int, s string) {
	if s == "" {
		return
	}
	w.tag(field, 2)
	w.uvarint(uint64(len(s)))
	w.b = append(w.b, s...)
}

// f32 writes a fixed32 float field, omitting it when zero.
func (w *pbuf) f32(field int, v float32) {
	if v == 0 {
		return
	}
	bits := math.Float32bits(v)
	w.tag(field, 5)
	w.b = append(w.b, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))
}

// msg writes a length-delimited sub-message, omitting it when the body is empty
// (an all-zero sub-message carries no information for a proto3 reader).
func (w *pbuf) msg(field int, body []byte) {
	if len(body) == 0 {
		return
	}
	w.tag(field, 2)
	w.uvarint(uint64(len(body)))
	w.b = append(w.b, body...)
}

// response decoding: just enough to read a ResCollector

// pread walks a protobuf body field by field.
type pread struct {
	b []byte
	i int
}

func (r *pread) varint() (uint64, bool) {
	var v uint64
	for shift := 0; r.i < len(r.b); shift += 7 {
		c := r.b[r.i]
		r.i++
		v |= uint64(c&0x7f) << shift
		if c < 0x80 {
			return v, true
		}
		if shift >= 63 {
			break
		}
	}
	return 0, false
}

// next returns the next field's number and wire type. For length-delimited and
// fixed fields it also returns the raw bytes. For varints it returns the value.
// ok is false at end of input or on a malformed field.
func (r *pread) next() (field, wire int, data []byte, val uint64, ok bool) {
	t, ok := r.varint()
	if !ok {
		return 0, 0, nil, 0, false
	}
	field, wire = int(t>>3), int(t&7)
	switch wire {
	case 0: // varint
		val, ok = r.varint()
		return field, wire, nil, val, ok
	case 1: // fixed64
		if r.i+8 > len(r.b) {
			return 0, 0, nil, 0, false
		}
		data = r.b[r.i : r.i+8]
		r.i += 8
		return field, wire, data, 0, true
	case 2: // length-delimited
		n, ok := r.varint()
		// The length is compared in uint64 space: a hostile length near 2^64
		// converted to int would pass a signed bounds check and panic the slice.
		if !ok || n > uint64(len(r.b)-r.i) {
			return 0, 0, nil, 0, false
		}
		data = r.b[r.i : r.i+int(n)]
		r.i += int(n)
		return field, wire, data, 0, true
	case 5: // fixed32
		if r.i+4 > len(r.b) {
			return 0, 0, nil, 0, false
		}
		data = r.b[r.i : r.i+4]
		r.i += 4
		return field, wire, data, 0, true
	default:
		return 0, 0, nil, 0, false
	}
}

// readResCollector pulls the realtime toggle out of a process-intake response.
// The intake answers each submission with a framed ResCollector (message type
// 23): ResCollector.Status (field 3) is a CollectorStatus whose ActiveClients
// (field 1) and Interval (field 2) tell us whether (and how often) to send the
// realtime stream. ok is true for any ResCollector, so the caller acts on it.
// It is false only when the response is not a ResCollector at all (in which case
// the caller leaves realtime as it was). An absent or all-zero Status reads as
// zero clients, i.e. turn realtime off, which is the safe default.
func readResCollector(framed []byte) (activeClients, interval int32, ok bool) {
	body, typ, ok := unframe(framed)
	if !ok || typ != typeResCollector {
		return 0, 0, false
	}
	r := &pread{b: body}
	for {
		field, _, data, _, more := r.next()
		if !more {
			return 0, 0, true // ResCollector with no Status: zero clients
		}
		if field == 3 { // Status
			activeClients, interval = readCollectorStatus(data)
			return activeClients, interval, true
		}
	}
}

func readCollectorStatus(b []byte) (activeClients, interval int32) {
	r := &pread{b: b}
	for {
		field, wire, _, val, more := r.next()
		if !more {
			return activeClients, interval
		}
		if wire != 0 {
			continue
		}
		switch field {
		case 1:
			activeClients = int32(val)
		case 2:
			interval = int32(val)
		}
	}
}
