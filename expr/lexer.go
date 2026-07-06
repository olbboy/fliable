// Package expr implements the Fliable expression language: a small, fast,
// deterministic and sandboxed language used for sequence flow conditions,
// script tasks, gateways, timers and variable mappings.
//
// Design goals, deliberately different from the JUEL/JavaScript engines
// used by Java BPM platforms:
//
//   - Sandboxed: expressions can only read the variables passed in and call
//     the registered pure functions. There is no reflection, no method
//     invocation, no I/O and no way to mutate engine state.
//   - Deterministic: same inputs, same outputs. now() is injected by the
//     caller so history replay stays exact.
//   - Fast: expressions compile once to an AST and are cached; evaluation
//     is allocation-light tree walking with no per-call parsing.
//
// Values are the JSON scalar universe: nil, bool, float64, string, []any,
// map[string]any, plus time.Time for temporal logic. All numeric inputs
// (int, int64, float32, json.Number, ...) are normalized to float64.
package expr

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

type tokenKind int

const (
	tokEOF tokenKind = iota
	tokNumber
	tokString
	tokIdent
	tokOp   // operators and punctuation
	tokBool // true / false
	tokNull // null / nil
)

type token struct {
	kind tokenKind
	val  string
	pos  int
}

// CompileError describes a syntax error with its byte offset in the source.
type CompileError struct {
	Src string
	Pos int
	Msg string
}

func (e *CompileError) Error() string {
	return fmt.Sprintf("expr: %s at offset %d in %q", e.Msg, e.Pos, e.Src)
}

type lexer struct {
	src    string
	pos    int
	tokens []token
}

var operators = []string{
	"==", "!=", "<=", ">=", "&&", "||", "??",
	"+", "-", "*", "/", "%", "<", ">", "!", "(", ")", "[", "]", ",", ".", "?", ":",
}

func lex(src string) ([]token, error) {
	l := &lexer{src: src}
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.pos++
		case c >= '0' && c <= '9':
			l.number()
		case c == '"' || c == '\'':
			if err := l.str(c); err != nil {
				return nil, err
			}
		case isIdentStart(rune(c)):
			l.ident()
		default:
			if !l.operator() {
				r, _ := utf8.DecodeRuneInString(l.src[l.pos:])
				return nil, &CompileError{Src: src, Pos: l.pos, Msg: fmt.Sprintf("unexpected character %q", r)}
			}
		}
	}
	l.tokens = append(l.tokens, token{kind: tokEOF, pos: l.pos})
	return l.tokens, nil
}

func (l *lexer) number() {
	start := l.pos
	seenDot := false
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		if c == '.' && !seenDot && l.pos+1 < len(l.src) && l.src[l.pos+1] >= '0' && l.src[l.pos+1] <= '9' {
			seenDot = true
			l.pos++
			continue
		}
		if c < '0' || c > '9' {
			break
		}
		l.pos++
	}
	l.tokens = append(l.tokens, token{kind: tokNumber, val: l.src[start:l.pos], pos: start})
}

func (l *lexer) str(quote byte) error {
	start := l.pos
	l.pos++ // opening quote
	var b strings.Builder
	for l.pos < len(l.src) {
		c := l.src[l.pos]
		switch c {
		case quote:
			l.pos++
			l.tokens = append(l.tokens, token{kind: tokString, val: b.String(), pos: start})
			return nil
		case '\\':
			l.pos++
			if l.pos >= len(l.src) {
				return &CompileError{Src: l.src, Pos: start, Msg: "unterminated string"}
			}
			switch e := l.src[l.pos]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '\\', '\'', '"':
				b.WriteByte(e)
			default:
				return &CompileError{Src: l.src, Pos: l.pos, Msg: fmt.Sprintf("unknown escape \\%c", e)}
			}
			l.pos++
		default:
			b.WriteByte(c)
			l.pos++
		}
	}
	return &CompileError{Src: l.src, Pos: start, Msg: "unterminated string"}
}

func (l *lexer) ident() {
	start := l.pos
	for l.pos < len(l.src) {
		r, size := utf8.DecodeRuneInString(l.src[l.pos:])
		if !isIdentPart(r) {
			break
		}
		l.pos += size
	}
	word := l.src[start:l.pos]
	switch word {
	case "true", "false":
		l.tokens = append(l.tokens, token{kind: tokBool, val: word, pos: start})
	case "null", "nil":
		l.tokens = append(l.tokens, token{kind: tokNull, val: word, pos: start})
	case "and":
		l.tokens = append(l.tokens, token{kind: tokOp, val: "&&", pos: start})
	case "or":
		l.tokens = append(l.tokens, token{kind: tokOp, val: "||", pos: start})
	case "not":
		l.tokens = append(l.tokens, token{kind: tokOp, val: "!", pos: start})
	case "in":
		l.tokens = append(l.tokens, token{kind: tokOp, val: "in", pos: start})
	default:
		l.tokens = append(l.tokens, token{kind: tokIdent, val: word, pos: start})
	}
}

func (l *lexer) operator() bool {
	for _, op := range operators {
		if strings.HasPrefix(l.src[l.pos:], op) {
			l.tokens = append(l.tokens, token{kind: tokOp, val: op, pos: l.pos})
			l.pos += len(op)
			return true
		}
	}
	return false
}

func isIdentStart(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

func isIdentPart(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
