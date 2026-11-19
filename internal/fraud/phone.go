// Package fraud holds the fraud-check provider clients and phone handling.
package fraud

import "strings"

// NormalizePhone turns the ways a Bangladeshi mobile number is typed
// (+8801712345678, 8801712-345678, 01712 345 678) into 01712345678.
// ok is false when the result is not a valid 01[3-9]XXXXXXXX number.
func NormalizePhone(raw string) (string, bool) {
	var b strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	switch {
	case strings.HasPrefix(s, "880") && len(s) == 13:
		s = "0" + s[3:]
	case len(s) == 10 && s[0] == '1':
		s = "0" + s
	}
	if len(s) != 11 || s[0] != '0' || s[1] != '1' || s[2] < '3' || s[2] > '9' {
		return "", false
	}
	return s, true
}
