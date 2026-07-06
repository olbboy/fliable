package expr

import "fmt"

// node is a compiled AST node.
type node interface {
	eval(env *envFrame) (any, error)
}

type litNode struct{ v any }

type varNode struct{ name string }

type listNode struct{ items []node }

type unaryNode struct {
	op string
	x  node
}

type binaryNode struct {
	op   string
	l, r node
}

type ternaryNode struct {
	cond, then, els node
}

type memberNode struct {
	x    node
	name string
}

type indexNode struct {
	x, idx node
}

type callNode struct {
	name string
	args []node
}

type parser struct {
	src  string
	toks []token
	pos  int
}

// Compile parses src into a reusable Program. Programs are immutable and
// safe for concurrent evaluation.
func Compile(src string) (*Program, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	p := &parser{src: src, toks: toks}
	n, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if t := p.peek(); t.kind != tokEOF {
		return nil, p.errf(t, "unexpected %q", t.val)
	}
	return &Program{src: src, root: n}, nil
}

func (p *parser) peek() token { return p.toks[p.pos] }
func (p *parser) next() token { t := p.toks[p.pos]; p.pos++; return t }
func (p *parser) accept(op string) bool {
	if t := p.peek(); t.kind == tokOp && t.val == op {
		p.pos++
		return true
	}
	return false
}

func (p *parser) expect(op string) error {
	if !p.accept(op) {
		t := p.peek()
		return p.errf(t, "expected %q, found %q", op, t.val)
	}
	return nil
}

func (p *parser) errf(t token, format string, args ...any) error {
	return &CompileError{Src: p.src, Pos: t.pos, Msg: fmt.Sprintf(format, args...)}
}

// parseExpr := ternary
func (p *parser) parseExpr() (node, error) {
	cond, err := p.parseCoalesce()
	if err != nil {
		return nil, err
	}
	if !p.accept("?") {
		return cond, nil
	}
	then, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	if err := p.expect(":"); err != nil {
		return nil, err
	}
	els, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &ternaryNode{cond: cond, then: then, els: els}, nil
}

// parseCoalesce := or ('??' or)*
func (p *parser) parseCoalesce() (node, error) {
	l, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	for p.accept("??") {
		r, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: "??", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseOr() (node, error) {
	l, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.accept("||") {
		r, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: "||", l: l, r: r}
	}
	return l, nil
}

func (p *parser) parseAnd() (node, error) {
	l, err := p.parseCmp()
	if err != nil {
		return nil, err
	}
	for p.accept("&&") {
		r, err := p.parseCmp()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: "&&", l: l, r: r}
	}
	return l, nil
}

var cmpOps = []string{"==", "!=", "<=", ">=", "<", ">", "in"}

func (p *parser) parseCmp() (node, error) {
	l, err := p.parseAdd()
	if err != nil {
		return nil, err
	}
	for {
		matched := false
		for _, op := range cmpOps {
			if p.accept(op) {
				r, err := p.parseAdd()
				if err != nil {
					return nil, err
				}
				l = &binaryNode{op: op, l: l, r: r}
				matched = true
				break
			}
		}
		if !matched {
			return l, nil
		}
	}
}

func (p *parser) parseAdd() (node, error) {
	l, err := p.parseMul()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.accept("+"):
			r, err := p.parseMul()
			if err != nil {
				return nil, err
			}
			l = &binaryNode{op: "+", l: l, r: r}
		case p.accept("-"):
			r, err := p.parseMul()
			if err != nil {
				return nil, err
			}
			l = &binaryNode{op: "-", l: l, r: r}
		default:
			return l, nil
		}
	}
}

func (p *parser) parseMul() (node, error) {
	l, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		var op string
		switch {
		case p.accept("*"):
			op = "*"
		case p.accept("/"):
			op = "/"
		case p.accept("%"):
			op = "%"
		default:
			return l, nil
		}
		r, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		l = &binaryNode{op: op, l: l, r: r}
	}
}

func (p *parser) parseUnary() (node, error) {
	if p.accept("!") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: "!", x: x}, nil
	}
	if p.accept("-") {
		x, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{op: "-", x: x}, nil
	}
	return p.parsePostfix()
}

func (p *parser) parsePostfix() (node, error) {
	x, err := p.parsePrimary()
	if err != nil {
		return nil, err
	}
	for {
		switch {
		case p.accept("."):
			t := p.next()
			if t.kind != tokIdent {
				return nil, p.errf(t, "expected property name after '.'")
			}
			x = &memberNode{x: x, name: t.val}
		case p.accept("["):
			idx, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expect("]"); err != nil {
				return nil, err
			}
			x = &indexNode{x: x, idx: idx}
		default:
			return x, nil
		}
	}
}

func (p *parser) parsePrimary() (node, error) {
	t := p.next()
	switch t.kind {
	case tokNumber:
		var f float64
		if _, err := fmt.Sscanf(t.val, "%g", &f); err != nil {
			return nil, p.errf(t, "bad number %q", t.val)
		}
		return &litNode{v: f}, nil
	case tokString:
		return &litNode{v: t.val}, nil
	case tokBool:
		return &litNode{v: t.val == "true"}, nil
	case tokNull:
		return &litNode{v: nil}, nil
	case tokIdent:
		if p.accept("(") {
			var args []node
			if !p.accept(")") {
				for {
					a, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					args = append(args, a)
					if p.accept(")") {
						break
					}
					if err := p.expect(","); err != nil {
						return nil, err
					}
				}
			}
			return &callNode{name: t.val, args: args}, nil
		}
		return &varNode{name: t.val}, nil
	case tokOp:
		switch t.val {
		case "(":
			x, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			if err := p.expect(")"); err != nil {
				return nil, err
			}
			return x, nil
		case "[":
			var items []node
			if !p.accept("]") {
				for {
					it, err := p.parseExpr()
					if err != nil {
						return nil, err
					}
					items = append(items, it)
					if p.accept("]") {
						break
					}
					if err := p.expect(","); err != nil {
						return nil, err
					}
				}
			}
			return &listNode{items: items}, nil
		}
	}
	return nil, p.errf(t, "unexpected %q", tokenLabel(t))
}

func tokenLabel(t token) string {
	if t.kind == tokEOF {
		return "end of expression"
	}
	return t.val
}
