/*
Copyright 2021 CodeNotary, Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sql

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

//go:generate go run golang.org/x/tools/cmd/goyacc -l -o sql_parser.go sql_grammar.y

var reservedWords = map[string]int{
	"CREATE":   CREATE,
	"USE":      USE,
	"DATABASE": DATABASE,
	"TABLE":    TABLE,
	"INDEX":    INDEX,
	"ON":       ON,
	"ALTER":    ALTER,
	"ADD":      ADD,
	"COLUMN":   COLUMN,
}

var types = map[string]struct{}{
	"INTEGER":   {},
	"BOOLEAN":   {},
	"STRING":    {},
	"BLOB":      {},
	"TIMESTAMP": {},
}

type lexer struct {
	r      *aheadByteReader
	err    error
	result []SQLStmt
}

type aheadByteReader struct {
	nextChar byte
	nextErr  error
	r        io.ByteReader
}

func newAheadByteReader(r io.ByteReader) *aheadByteReader {
	ar := &aheadByteReader{r: r}
	ar.nextChar, ar.nextErr = r.ReadByte()
	return ar
}

func (ar *aheadByteReader) ReadByte() (byte, error) {
	defer func() {
		if ar.nextErr == nil {
			ar.nextChar, ar.nextErr = ar.r.ReadByte()
		}
	}()

	return ar.nextChar, ar.nextErr
}

func (ar *aheadByteReader) NextByte() (byte, error) {
	return ar.nextChar, ar.nextErr
}

func ParseString(sql string) ([]SQLStmt, error) {
	return Parse(strings.NewReader(sql))
}

func ParseBytes(sql []byte) ([]SQLStmt, error) {
	return Parse(bytes.NewReader(sql))
}

func Parse(r io.ByteReader) ([]SQLStmt, error) {
	lexer := newLexer(r)
	yyErrorVerbose = true

	yyParse(lexer)

	return lexer.result, lexer.err
}

func newLexer(r io.ByteReader) *lexer {
	return &lexer{
		r:   newAheadByteReader(r),
		err: nil,
	}
}

func (l *lexer) Lex(lval *yySymType) int {
	var ch byte
	var err error

	for {
		ch, err = l.r.ReadByte()
		if err == io.EOF {
			return 0
		}
		if err != nil {
			lval.err = err
			return ERROR
		}

		if !isSpace(ch) {
			break
		}
	}

	if isSeparator(ch) {
		if ch == '\r' && l.r.nextChar == '\n' {
			l.r.ReadByte()
		}
		return STMT_SEPARATOR
	}

	if isLetter(ch) {
		tail, err := l.readWord()
		if err != nil {
			lval.err = err
			return ERROR
		}

		lval.id = fmt.Sprintf("%c%s", ch, tail)

		if isType(lval.id) {
			return TYPE
		}

		tkn, ok := reservedWords[lval.id]
		if !ok {
			return ID
		}

		return tkn
	}

	return int(ch)
}

func (l *lexer) readWord() (string, error) {
	var b bytes.Buffer

	for {
		ch, err := l.r.NextByte()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		if !isLetter(ch) && !isNumber(ch) {
			break
		}

		ch, _ = l.r.ReadByte()
		b.WriteByte(ch)
	}

	return b.String(), nil
}

func isType(id string) bool {
	_, ok := types[id]
	return ok
}

func isSeparator(ch byte) bool {
	return ';' == ch || '\r' == ch || '\n' == ch
}

func isSpace(ch byte) bool {
	return ' ' == ch
}

func isNumber(ch byte) bool {
	return '0' <= ch && ch <= '9'
}

func isLetter(ch byte) bool {
	return 'a' <= ch && ch <= 'z' || 'A' <= ch && ch <= 'Z' || ch == '_'
}

func (l *lexer) Error(err string) {
	l.err = errors.New(err)
}
