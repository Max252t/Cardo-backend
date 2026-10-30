package rank

import (
	"errors"
	"fmt"
	"strings"
)

const (
	Digits         = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	MaxLength      = 64
	base           = len(Digits)
	zero      byte = '0'
	maxDigit  byte = 'z'
)

var smallestInteger = "A" + strings.Repeat(string(zero), 26)

var (
	ErrInvalidKey = errors.New("rank: invalid key")
	ErrOrder      = errors.New("rank: lower bound must be less than upper bound")
	ErrExhausted  = errors.New("rank: key space exhausted")
)

func digitIndex(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 36
	}
	return -1
}

func integerLength(head byte) (int, error) {
	switch {
	case head >= 'a' && head <= 'z':
		return int(head-'a') + 2, nil
	case head >= 'A' && head <= 'Z':
		return int('Z'-head) + 2, nil
	}
	return 0, fmt.Errorf("%w: head %q", ErrInvalidKey, head)
}

func integerPart(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidKey)
	}
	n, err := integerLength(key[0])
	if err != nil {
		return "", err
	}
	if n > len(key) {
		return "", fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return key[:n], nil
}

func Validate(key string) error {
	if key == smallestInteger {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	for i := 0; i < len(key); i++ {
		if digitIndex(key[i]) < 0 {
			return fmt.Errorf("%w: %q", ErrInvalidKey, key)
		}
	}
	ip, err := integerPart(key)
	if err != nil {
		return err
	}
	if f := key[len(ip):]; f != "" && f[len(f)-1] == zero {
		return fmt.Errorf("%w: trailing zero in %q", ErrInvalidKey, key)
	}
	return nil
}

func midpoint(a, b string, bounded bool) string {
	if bounded {
		n := 0
		for n < len(b) && digitAt(a, n) == b[n] {
			n++
		}
		if n > 0 {
			return b[:n] + midpoint(suffix(a, n), b[n:], true)
		}
	}
	digitA := 0
	if a != "" {
		digitA = digitIndex(a[0])
	}
	digitB := base
	if bounded {
		digitB = digitIndex(b[0])
	}
	if digitB-digitA > 1 {
		return string(Digits[(digitA+digitB+1)/2])
	}
	if bounded && len(b) > 1 {
		return b[:1]
	}
	return string(Digits[digitA]) + midpoint(suffix(a, 1), "", false)
}

func digitAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return zero
}

func suffix(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

func incrementInteger(x string) (string, bool) {
	head := x[0]
	digs := []byte(x[1:])
	carry := true
	for i := len(digs) - 1; carry && i >= 0; i-- {
		d := digitIndex(digs[i]) + 1
		if d == base {
			digs[i] = zero
		} else {
			digs[i] = Digits[d]
			carry = false
		}
	}
	if !carry {
		return string(head) + string(digs), true
	}
	switch head {
	case 'Z':
		return "a" + string(zero), true
	case 'z':
		return "", false
	}
	h := head + 1
	if h > 'a' {
		digs = append(digs, zero)
	} else {
		digs = digs[:len(digs)-1]
	}
	return string(h) + string(digs), true
}

func decrementInteger(x string) (string, bool) {
	head := x[0]
	digs := []byte(x[1:])
	borrow := true
	for i := len(digs) - 1; borrow && i >= 0; i-- {
		d := digitIndex(digs[i]) - 1
		if d == -1 {
			digs[i] = maxDigit
		} else {
			digs[i] = Digits[d]
			borrow = false
		}
	}
	if !borrow {
		return string(head) + string(digs), true
	}
	switch head {
	case 'a':
		return "Z" + string(maxDigit), true
	case 'A':
		return "", false
	}
	h := head - 1
	if h < 'Z' {
		digs = append(digs, maxDigit)
	} else {
		digs = digs[:len(digs)-1]
	}
	return string(h) + string(digs), true
}

func Between(a, b string) (string, error) {
	if a != "" {
		if err := Validate(a); err != nil {
			return "", err
		}
	}
	if b != "" {
		if err := Validate(b); err != nil {
			return "", err
		}
	}
	if a != "" && b != "" && a >= b {
		return "", fmt.Errorf("%w: %q >= %q", ErrOrder, a, b)
	}
	switch {
	case a == "" && b == "":
		return "a" + string(zero), nil
	case a == "":
		return before(b)
	case b == "":
		return after(a)
	}
	return between(a, b)
}

func before(b string) (string, error) {
	ib, _ := integerPart(b)
	fb := b[len(ib):]
	if ib == smallestInteger {
		return ib + midpoint("", fb, true), nil
	}
	if ib < b {
		return ib, nil
	}
	res, ok := decrementInteger(ib)
	if !ok {
		return "", ErrExhausted
	}
	return res, nil
}

func after(a string) (string, error) {
	ia, _ := integerPart(a)
	fa := a[len(ia):]
	if i, ok := incrementInteger(ia); ok {
		return i, nil
	}
	return ia + midpoint(fa, "", false), nil
}

func between(a, b string) (string, error) {
	ia, _ := integerPart(a)
	fa := a[len(ia):]
	ib, _ := integerPart(b)
	fb := b[len(ib):]
	if ia == ib {
		return ia + midpoint(fa, fb, true), nil
	}
	i, ok := incrementInteger(ia)
	if !ok {
		return "", ErrExhausted
	}
	if i < b {
		return i, nil
	}
	return ia + midpoint(fa, "", false), nil
}

func NBetween(a, b string, n int) ([]string, error) {
	switch {
	case n <= 0:
		return []string{}, nil
	case n == 1:
		k, err := Between(a, b)
		if err != nil {
			return nil, err
		}
		return []string{k}, nil
	case b == "":
		return sequence(a, n, func(prev string) (string, error) { return Between(prev, "") })
	case a == "":
		keys, err := sequence(b, n, func(next string) (string, error) { return Between("", next) })
		if err != nil {
			return nil, err
		}
		for i, j := 0, len(keys)-1; i < j; i, j = i+1, j-1 {
			keys[i], keys[j] = keys[j], keys[i]
		}
		return keys, nil
	}
	mid := n / 2
	c, err := Between(a, b)
	if err != nil {
		return nil, err
	}
	left, err := NBetween(a, c, mid)
	if err != nil {
		return nil, err
	}
	right, err := NBetween(c, b, n-mid-1)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, n)
	out = append(out, left...)
	out = append(out, c)
	return append(out, right...), nil
}

func sequence(start string, n int, next func(string) (string, error)) ([]string, error) {
	out := make([]string, 0, n)
	cur := start
	for range n {
		k, err := next(cur)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
		cur = k
	}
	return out, nil
}

func Evenly(n int) ([]string, error) {
	return NBetween("", "", n)
}
