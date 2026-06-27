package main

// A read-only protobuf field-walker, just enough to pull the process list out of
// a plain (encoding 0) CollectorProc frame. Mirrors internal/process/proto.go's
// reader. Field numbers match internal/process/payload.go's encoder: CollectorProc
// carries Process at field 3. Within a Process, Command is field 4. Within a
// Command, Comm is field 9.
const typeCollectorProc = 12

// pwalk iterates a protobuf body, invoking fn for each field. For varint fields
// (wire 0) val holds the value. For length-delimited fields (wire 2) data holds
// the raw bytes. It stops at the first malformed field.
func pwalk(b []byte, fn func(field, wire int, data []byte, val uint64)) {
	i := 0
	varint := func() (uint64, bool) {
		var v uint64
		for shift := 0; i < len(b); shift += 7 {
			c := b[i]
			i++
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
	for i < len(b) {
		tag, ok := varint()
		if !ok {
			return
		}
		field, wire := int(tag>>3), int(tag&7)
		switch wire {
		case 0: // varint
			v, ok := varint()
			if !ok {
				return
			}
			fn(field, wire, nil, v)
		case 1: // fixed64
			if i+8 > len(b) {
				return
			}
			fn(field, wire, b[i:i+8], 0)
			i += 8
		case 2: // length-delimited
			n, ok := varint()
			// Compared in uint64 space: a hostile length near 2^64 converted
			// to int would pass a signed bounds check and panic the slice.
			if !ok || n > uint64(len(b)-i) {
				return
			}
			fn(field, wire, b[i:i+int(n)], 0)
			i += int(n)
		case 5: // fixed32
			if i+4 > len(b) {
				return
			}
			fn(field, wire, b[i:i+4], 0)
			i += 4
		default:
			return
		}
	}
}

// decodeCollectorProc returns the process count and command names from a plain
// CollectorProc body.
func decodeCollectorProc(body []byte) (count int, names []string) {
	pwalk(body, func(field, wire int, data []byte, _ uint64) {
		if field == 3 && wire == 2 { // a Process
			count++
			if name := processComm(data); name != "" {
				names = append(names, name)
			}
		}
	})
	return count, names
}

// processComm pulls Command.Comm (Process field 4 -> Command field 9) from a
// Process message.
func processComm(proc []byte) string {
	var comm string
	pwalk(proc, func(field, wire int, data []byte, _ uint64) {
		if field == 4 && wire == 2 { // Command
			pwalk(data, func(f, w int, d []byte, _ uint64) {
				if f == 9 && w == 2 { // Comm
					comm = string(d)
				}
			})
		}
	})
	return comm
}
