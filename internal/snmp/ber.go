package snmp

import (
	"strconv"
	"strings"
)

// tlv wraps value in a BER Tag-Length-Value header. Length uses definite
// short or long form as required (RFC not needing indefinite form for our
// fixed-size messages).
func tlv(tag byte, value []byte) []byte {
	out := []byte{tag}
	out = append(out, encodeLength(len(value))...)
	out = append(out, value...)
	return out
}

func encodeLength(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte(n & 0xff)}, b...)
		n >>= 8
	}
	return append([]byte{byte(0x80 | len(b))}, b...)
}

// sequence wraps content in a SEQUENCE (0x30).
func sequence(content []byte) []byte { return tlv(0x30, content) }

// integer BER-encodes a non-negative INTEGER (0x02).
func integer(v int64) []byte {
	if v == 0 {
		return tlv(0x02, []byte{0})
	}
	var b []byte
	neg := v < 0
	uv := uint64(v)
	if neg {
		uv = uint64(-v)
	}
	for uv > 0 {
		b = append([]byte{byte(uv & 0xff)}, b...)
		uv >>= 8
	}
	if !neg && b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return tlv(0x02, b)
}

// octetString BER-encodes an OCTET STRING (0x04).
func octetString(s string) []byte { return tlv(0x04, []byte(s)) }

// timeTicks BER-encodes an application-tagged TimeTicks (hundredths of a
// second) value, used for sysUpTime.0.
func timeTicksRaw(hundredths uint32) []byte {
	b := []byte{byte(hundredths >> 24), byte(hundredths >> 16), byte(hundredths >> 8), byte(hundredths)}
	// strip leading zero bytes but keep at least one, and keep MSB
	// unambiguous (unsigned semantics, so no sign-bit padding needed here).
	i := 0
	for i < 3 && b[i] == 0 {
		i++
	}
	return tlv(0x43, b[i:])
}

func timeTicks(d interface{ Seconds() float64 }) []byte {
	return timeTicksRaw(uint32(d.Seconds() * 100))
}

// oidEncode BER-encodes an OBJECT IDENTIFIER value's content bytes (no tag).
func oidEncode(oid string) []byte {
	parts := strings.Split(strings.Trim(oid, "."), ".")
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		n, _ := strconv.ParseUint(p, 10, 64)
		nums[i] = n
	}
	var out []byte
	if len(nums) >= 2 {
		out = append(out, byte(nums[0]*40+nums[1]))
		nums = nums[2:]
	} else if len(nums) == 1 {
		out = append(out, byte(nums[0]*40))
		nums = nil
	}
	for _, n := range nums {
		out = append(out, encodeBase128(n)...)
	}
	return out
}

// oidValue returns the full TLV (tag 0x06) for an OID, used as a varbind
// value or standalone element.
func oidValue(oid string) []byte { return tlv(0x06, oidEncode(oid)) }

func encodeBase128(n uint64) []byte {
	if n == 0 {
		return []byte{0}
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte(n & 0x7f)}, b...)
		n >>= 7
	}
	for i := 0; i < len(b)-1; i++ {
		b[i] |= 0x80
	}
	return b
}
