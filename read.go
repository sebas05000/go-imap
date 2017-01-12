package imap

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
)

const (
	sp            = ' '
	cr            = '\r'
	lf            = '\n'
	dquote        = '"'
	literalStart  = '{'
	literalEnd    = '}'
	listStart     = '('
	listEnd       = ')'
	respCodeStart = '['
	respCodeEnd   = ']'
)

const (
	crlf    = "\r\n"
	nilAtom = "NIL"
)

// TODO: add CTL to atomSpecials
var (
	quotedSpecials = string([]rune{dquote, '\\'})
	respSpecials   = string([]rune{respCodeEnd})
	atomSpecials   = string([]rune{listStart, listEnd, literalStart, sp, '%', '*'}) + quotedSpecials + respSpecials
)

type parseError struct {
	error
}

func newParseError(text string) error {
	return &parseError{errors.New(text)}
}

// IsParseError returns true if the provided error is a parse error produced by
// Reader.
func IsParseError(err error) bool {
	_, ok := err.(*parseError)
	return ok
}

// A string reader.
type StringReader interface {
	// ReadString reads until the first occurrence of delim in the input,
	// returning a string containing the data up to and including the delimiter.
	// See https://golang.org/pkg/bufio/#Reader.ReadString
	ReadString(delim byte) (line string, err error)
}

type reader interface {
	io.Reader
	io.RuneScanner
	StringReader
}

// Convert a field to a number.
func ParseNumber(f interface{}) (uint32, error) {
	// Useful for tests
	if n, ok := f.(uint32); ok {
		return n, nil
	}

	s, ok := f.(string)
	if !ok {
		return 0, newParseError("number is not a string")
	}

	nbr, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, &parseError{err}
	}

	return uint32(nbr), nil
}

// Convert a field list to a string list.
func ParseStringList(f interface{}) ([]string, error) {
	fields, ok := f.([]interface{})
	if !ok {
		return nil, newParseError("string list is not a list")
	}

	list := make([]string, len(fields))
	for i, f := range fields {
		var ok bool
		if list[i], ok = f.(string); !ok {
			return nil, newParseError("string list contains a non-string")
		}
	}
	return list, nil
}

func trimSuffix(str string, suffix rune) string {
	return str[:len(str)-1]
}

// An IMAP reader.
type Reader struct {
	MaxLiteralSize uint32 // The maximum literal size.

	reader

	continues      chan<- bool

	brackets   int
	inRespCode bool
}

func (r *Reader) ReadSp() error {
	char, _, err := r.ReadRune()
	if err != nil {
		return err
	}
	if char != sp {
		return newParseError("not a space")
	}
	return nil
}

func (r *Reader) ReadCrlf() (err error) {
	var char rune

	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char != cr {
		err = newParseError("line doesn't end with a CR")
	}

	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char != lf {
		err = newParseError("line doesn't end with a LF")
	}

	return
}

func (r *Reader) ReadAtom() (interface{}, error) {
	r.brackets = 0

	var atom string
	for {
		char, _, err := r.ReadRune()
		if err != nil {
			return nil, err
		}

		// TODO: list-wildcards and \
		if r.brackets == 0 && (char == listStart || char == literalStart || char == dquote) {
			return nil, newParseError("atom contains forbidden char: " + string(char))
		}
		if char == cr {
			break
		}
		if r.brackets == 0 && (char == sp || char == listEnd) {
			break
		}
		if char == respCodeEnd {
			if r.brackets == 0 {
				if r.inRespCode {
					break
				} else {
					return nil, newParseError("atom contains bad brackets nesting")
				}
			}
			r.brackets--
		}
		if char == respCodeStart {
			r.brackets++
		}

		atom += string(char)
	}

	r.UnreadRune()

	if atom == "NIL" {
		return nil, nil
	}
	return atom, nil
}

func (r *Reader) ReadLiteral() (Literal, error) {
	char, _, err := r.ReadRune()
	if err != nil {
		return nil, err
	} else if char != literalStart {
		return nil, newParseError("literal string doesn't start with an open brace")
	}

	lstr, err := r.ReadString(byte(literalEnd))
	if err != nil {
		return nil, err
	}
	lstr = trimSuffix(lstr, literalEnd)
	n, err := strconv.ParseUint(lstr, 10, 32)
	if err != nil {
		return nil, newParseError("cannot parse literal length: " + err.Error())
	}
	if r.MaxLiteralSize > 0 && uint32(n) > r.MaxLiteralSize {
		return nil, newParseError("literal exceeding maximum size")
	}

	if err := r.ReadCrlf(); err != nil {
		return nil, err
	}

	// Send continuation request if necessary
	if r.continues != nil {
		r.continues <- true
	}

	// Read literal
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return bytes.NewBuffer(b), nil
}

func (r *Reader) ReadQuotedString() (string, error) {
	if char, _, err := r.ReadRune(); err != nil {
		return "", err
	} else if char != dquote {
		return "", newParseError("quoted string doesn't start with a double quote")
	}

	var buf bytes.Buffer
	var escaped bool
	for {
		char, _, err := r.ReadRune()
		if err != nil {
			return "", err
		}

		if char == '\\' && !escaped {
			escaped = true
		} else {
			if char == cr || char == lf {
				r.UnreadRune()
				return "", newParseError("CR or LF not allowed in quoted string")
			}
			if char == dquote && !escaped {
				break
			}

			if !strings.ContainsRune(quotedSpecials, char) && escaped {
				return "", newParseError("quoted string cannot contain backslash followed by a non-quoted-specials char")
			}

			buf.WriteRune(char)
			escaped = false
		}
	}

	return buf.String(), nil
}

func (r *Reader) ReadFields() (fields []interface{}, err error) {
	var char rune
	for {
		if char, _, err = r.ReadRune(); err != nil {
			return
		}
		if err = r.UnreadRune(); err != nil {
			return
		}

		var field interface{}
		ok := true
		switch char {
		case literalStart:
			field, err = r.ReadLiteral()
		case dquote:
			field, err = r.ReadQuotedString()
		case listStart:
			field, err = r.ReadList()
		case listEnd:
			ok = false
		case cr:
			return
		default:
			field, err = r.ReadAtom()
		}

		if err != nil {
			return
		}
		if ok {
			fields = append(fields, field)
		}

		if char, _, err = r.ReadRune(); err != nil {
			return
		}
		if char == cr || char == listEnd || char == respCodeEnd {
			return
		}
		if char == listStart {
			r.UnreadRune()
			continue
		}
		if char != sp {
			err = newParseError("fields are not separated by a space")
			return
		}
	}
}

func (r *Reader) ReadList() (fields []interface{}, err error) {
	char, _, err := r.ReadRune()
	if err != nil {
		return
	}
	if char != listStart {
		err = newParseError("list doesn't start with an open parenthesis")
		return
	}

	fields, err = r.ReadFields()
	if err != nil {
		return
	}

	r.UnreadRune()
	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char != listEnd {
		err = newParseError("list doesn't end with a close parenthesis")
	}
	return
}

func (r *Reader) ReadLine() (fields []interface{}, err error) {
	fields, err = r.ReadFields()
	if err != nil {
		return
	}

	r.UnreadRune()
	err = r.ReadCrlf()
	return
}

func (r *Reader) ReadRespCode() (code string, fields []interface{}, err error) {
	char, _, err := r.ReadRune()
	if err != nil {
		return
	}
	if char != respCodeStart {
		err = newParseError("response code doesn't start with an open bracket")
		return
	}

	r.inRespCode = true
	fields, err = r.ReadFields()
	r.inRespCode = false
	if err != nil {
		return
	}

	if len(fields) == 0 {
		err = newParseError("response code doesn't contain any field")
		return
	}

	code, ok := fields[0].(string)
	if !ok {
		err = newParseError("response code doesn't start with a string atom")
		return
	}
	if code == "" {
		err = newParseError("response code is empty")
		return
	}

	fields = fields[1:]

	r.UnreadRune()
	char, _, err = r.ReadRune()
	if err != nil {
		return
	}
	if char != respCodeEnd {
		err = newParseError("response code doesn't end with a close bracket")
	}
	return
}

func (r *Reader) ReadInfo() (info string, err error) {
	info, err = r.ReadString(byte(cr))
	if err != nil {
		return
	}
	info = strings.TrimSuffix(info, string(cr))
	info = strings.TrimLeft(info, " ")

	var char rune
	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char != lf {
		err = newParseError("line doesn't end with a LF")
	}
	return
}

func NewReader(r reader) *Reader {
	return &Reader{reader: r}
}

func NewServerReader(r reader, continues chan<- bool) *Reader {
	return &Reader{reader: r, continues: continues}
}

type Parser interface {
	Parse(fields []interface{}) error
}
