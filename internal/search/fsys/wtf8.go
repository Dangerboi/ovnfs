package fsys

import "unicode/utf8"

// WTF-8 conversions for Windows names. They are platform-neutral so their
// round-trip behavior is tested on every platform.

// appendWTF8 encodes UTF-16 as WTF-8: UTF-8, except unpaired surrogates are
// kept as their 3-byte generalized encoding so names round-trip exactly.
func appendWTF8(dst []byte, src []uint16) []byte {
	for i := 0; i < len(src); i++ {
		c := rune(src[i])
		switch {
		case c < utf8.RuneSelf:
			dst = append(dst, byte(c))
			continue
		case c >= 0xD800 && c < 0xDC00 && i+1 < len(src) && src[i+1] >= 0xDC00 && src[i+1] < 0xE000:
			c = 0x10000 + (c-0xD800)<<10 + rune(src[i+1]) - 0xDC00
			i++
		case c >= 0xD800 && c < 0xE000:
			dst = append(dst, 0xE0|byte(c>>12), 0x80|byte(c>>6)&0x3F, 0x80|byte(c)&0x3F)
			continue
		}
		dst = utf8.AppendRune(dst, c)
	}
	return dst
}

// appendUTF16 decodes WTF-8 (see [appendWTF8]) to UTF-16, mapping '/' to
// '\' so multi-component relative names resolve under the NT namespace.
func appendUTF16(dst []uint16, s string) []uint16 { return decodeWTF8(dst, s, true) }

// appendUTF16Raw decodes WTF-8 to UTF-16 without touching separators, for
// paths handed to Win32, which normalizes them itself except behind the
// verbatim `\\?\` prefix, where every byte must reach the kernel as spelled.
func appendUTF16Raw(dst []uint16, s string) []uint16 { return decodeWTF8(dst, s, false) }

func decodeWTF8(dst []uint16, s string, slashes bool) []uint16 {
	for i := 0; i < len(s); {
		b := s[i]
		switch {
		case b == '/' && slashes:
			dst = append(dst, '\\')
			i++
			continue
		case b < utf8.RuneSelf:
			dst = append(dst, uint16(b))
			i++
			continue
		case b == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF:
			// Generalized encoding of a lone surrogate.
			dst = append(dst, 0xD000|uint16(s[i+1]&0x3F)<<6|uint16(s[i+2]&0x3F))
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r >= 0x10000 {
			r -= 0x10000
			dst = append(dst, 0xD800+uint16(r>>10), 0xDC00+uint16(r&0x3FF))
		} else {
			dst = append(dst, uint16(r))
		}
	}
	return dst
}
