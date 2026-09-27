package web

import (
	"strings"
	"unicode/utf8"
)

// Punycode (RFC 3492) encoder for internationalized domain labels, so the
// app doesn't need golang.org/x/net/idna. Only encoding is implemented:
// configs are written with ASCII (A-label) names.

const (
	pcBase        = 36
	pcTMin        = 1
	pcTMax        = 26
	pcSkew        = 38
	pcDamp        = 700
	pcInitialBias = 72
	pcInitialN    = 128
)

func pcAdapt(delta, numPoints int32, first bool) int32 {
	if first {
		delta /= pcDamp
	} else {
		delta /= 2
	}
	delta += delta / numPoints
	k := int32(0)
	for delta > ((pcBase-pcTMin)*pcTMax)/2 {
		delta /= pcBase - pcTMin
		k += pcBase
	}
	return k + (pcBase-pcTMin+1)*delta/(delta+pcSkew)
}

func pcDigit(d int32) byte {
	if d < 26 {
		return byte('a' + d)
	}
	return byte('0' + d - 26)
}

// punyEncode encodes one label (without the "xn--" prefix). ok=false on
// overflow or invalid UTF-8.
func punyEncode(label string) (string, bool) {
	if !utf8.ValidString(label) {
		return "", false
	}
	runes := []rune(label)
	var out strings.Builder
	for _, r := range runes {
		if r < 0x80 {
			out.WriteByte(byte(r))
		}
	}
	b := int32(out.Len())
	h := b
	if b > 0 {
		out.WriteByte('-')
	}
	n := int32(pcInitialN)
	delta := int32(0)
	bias := int32(pcInitialBias)
	total := int32(len(runes))
	for h < total {
		m := int32(0x7fffffff)
		for _, r := range runes {
			if r >= n && r < m {
				m = r
			}
		}
		if (m - n) > (0x7fffffff-delta)/(h+1) {
			return "", false
		}
		delta += (m - n) * (h + 1)
		n = m
		for _, r := range runes {
			if r < n {
				delta++
				if delta < 0 {
					return "", false
				}
			}
			if r == n {
				q := delta
				for k := int32(pcBase); ; k += pcBase {
					t := k - bias
					if t < pcTMin {
						t = pcTMin
					} else if t > pcTMax {
						t = pcTMax
					}
					if q < t {
						break
					}
					out.WriteByte(pcDigit(t + (q-t)%(pcBase-t)))
					q = (q - t) / (pcBase - t)
				}
				out.WriteByte(pcDigit(q))
				bias = pcAdapt(delta, h+1, h == b)
				delta = 0
				h++
			}
		}
		delta++
		n++
	}
	return out.String(), true
}

// toASCIIHost converts a (possibly internationalized) host name to its
// lower-case ASCII form, label by label.
func toASCIIHost(host string) (string, bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	// Full-width and ideographic dots separate labels too (UTS #46).
	host = strings.NewReplacer("。", ".", "．", ".", "｡", ".").Replace(host)
	labels := strings.Split(host, ".")
	for i, l := range labels {
		ascii := true
		for j := 0; j < len(l); j++ {
			if l[j] >= 0x80 {
				ascii = false
				break
			}
		}
		if ascii {
			continue
		}
		enc, ok := punyEncode(l)
		if !ok {
			return "", false
		}
		labels[i] = "xn--" + enc
	}
	return strings.Join(labels, "."), true
}
