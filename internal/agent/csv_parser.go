package agent

import (
	"bufio"
	"context"
	"errors"
	"io"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/transform"
)

// csvCharacters validates every encoded character before CSV syntax processing.
// It does not remove BOMs, replace invalid bytes, or normalize quoted newlines.
type csvCharacters struct {
	ctx      context.Context
	r        *bufio.Reader
	encoding string
	decoder  transform.Transformer
}

func newCSVCharacters(ctx context.Context, r io.Reader, encoding string) *csvCharacters {
	return &csvCharacters{ctx: ctx, r: bufio.NewReader(r), encoding: encoding, decoder: japanese.ShiftJIS.NewDecoder()}
}
func (c *csvCharacters) next() (rune, error) {
	if e := c.ctx.Err(); e != nil {
		return 0, e
	}
	if c.encoding == "utf-8" {
		r, n, e := c.r.ReadRune()
		if e == nil && r == utf8.RuneError && n == 1 {
			return 0, errors.New("invalid UTF-8 CSV")
		}
		return r, e
	}
	b, e := c.r.ReadByte()
	if e != nil {
		return 0, e
	}
	if b < 0x80 {
		return rune(b), nil
	}
	if b >= 0xa1 && b <= 0xdf {
		return rune(b) + 0xff61 - 0xa1, nil
	}
	if c.encoding == "cp932" {
		if b == 0x80 {
			return 0x80, nil
		}
		if b == 0xa0 {
			return 0xf8f0, nil
		}
		if b >= 0xfd {
			return 0xf8f1 + rune(b-0xfd), nil
		}
	}
	validLead := b >= 0x81 && b <= 0x9f || b >= 0xe0 && b <= 0xfc
	if !validLead || c.encoding == "shift_jis" && (b == 0x87 || b > 0xea) {
		return 0, errors.New("invalid CSV encoding lead")
	}
	trail, e := c.r.ReadByte()
	if e != nil {
		if e == io.EOF {
			e = io.ErrUnexpectedEOF
		}
		return 0, e
	}
	if trail < 0x40 || trail == 0x7f || trail > 0xfc {
		return 0, errors.New("invalid CSV encoding trail")
	}
	if b >= 0xf0 && b <= 0xf9 {
		offset := int(trail) - 0x40
		if trail >= 0x80 {
			offset--
		}
		return rune(0xe000 + int(b-0xf0)*188 + offset), nil
	}
	raw := [2]byte{b, trail}
	var out [4]byte
	c.decoder.Reset()
	n, used, e := c.decoder.Transform(out[:], raw[:], true)
	if e != nil || used != 2 {
		return 0, errors.New("invalid CSV encoding pair")
	}
	r, _ := utf8.DecodeRune(out[:n])
	if r == utf8.RuneError {
		return 0, errors.New("unassigned CSV encoding pair")
	}
	if c.encoding == "shift_jis" {
		switch uint16(b)<<8 | uint16(trail) {
		case 0x8160:
			r = 0x301c
		case 0x8161:
			r = 0x2016
		case 0x817c:
			r = 0x2212
		case 0x8191:
			r = 0x00a2
		case 0x8192:
			r = 0x00a3
		case 0x81ca:
			r = 0x00ac
		}
	}
	return r, nil
}

type csvRecords struct {
	chars     *csvCharacters
	delimiter rune
}

// read follows strict RFC4180 quoting, accepts LF/CRLF record boundaries and
// preserves every CR/LF inside quotes. Blank physical lines are ignored.
func (p *csvRecords) read() ([]string, error) {
	for {
		fields := []string{}
		value := []rune{}
		state := byte(0)
		started := false
		finish := func() error {
			fields = append(fields, string(value))
			value = nil
			state = 0
			return nil
		}
		for {
			r, e := p.chars.next()
			if e != nil {
				if e != io.EOF {
					return nil, e
				}
				if state == 1 {
					return nil, errors.New("unterminated CSV quote")
				}
				if !started {
					return nil, io.EOF
				}
				if e = finish(); e != nil {
					return nil, e
				}
				return fields, nil
			}
			if state == 1 {
				if r == '"' {
					state = 2
				} else {
					value = append(value, r)
				}
				continue
			}
			if state == 2 && r == '"' {
				value = append(value, r)
				state = 1
				continue
			}
			if r == p.delimiter {
				started = true
				if e = finish(); e != nil {
					return nil, e
				}
				continue
			}
			if r == '\n' || r == '\r' {
				if r == '\r' {
					next, err := p.chars.next()
					if err != nil && err != io.EOF {
						return nil, err
					}
					if err == io.EOF || next != '\n' {
						return nil, errors.New("bare CR outside quoted CSV field")
					}
				}
				if !started {
					break
				}
				if e = finish(); e != nil {
					return nil, e
				}
				return fields, nil
			}
			if state == 2 {
				return nil, errors.New("unexpected character after CSV quote")
			}
			if r == '"' {
				if state != 0 {
					return nil, errors.New("quote in unquoted CSV field")
				}
				state = 1
				started = true
				continue
			}
			state = 3
			started = true
			value = append(value, r)
		}
	}
}

type csvLimit struct{}

func (*csvLimit) Error() string { return "CSV exceeds configured resource limits" }
