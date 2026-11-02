package starlsp

import (
	"strings"

	gotreesitter "github.com/odvcencio/gotreesitter"
)

// cursorContext is what the structural half can tell the semantic half about
// where the caret sits.
//
// The receiver chain is read lexically rather than from the tree. At the moment
// completion is requested the buffer usually reads `p.` — an expression the
// grammar cannot complete — so the tree at that offset is an ERROR node. The
// characters to the left of the caret are unambiguous even then.
type cursorContext struct {
	// Prefix is the partial identifier being typed, if any.
	Prefix string

	// Receiver is the dotted expression left of the caret's dot, empty when
	// the caret is not after a dot. For `fs.path("x").read` it is
	// `fs.path("x")`.
	Receiver string

	// AfterDot reports whether the caret follows a member access.
	AfterDot bool

	// InString reports whether the caret sits inside a string literal, where
	// identifier completion would be noise.
	InString bool

	// InComment reports whether the caret sits inside a comment.
	InComment bool

	// Call is the innermost call the caret sits inside, for signature help.
	Call *gotreesitter.Node

	// ArgIndex is the zero-based argument position inside Call.
	ArgIndex int
}

// contextAt inspects the caret position.
func (s *Server) contextAt(d *Document, offset int) cursorContext {
	ctx := cursorContext{ArgIndex: -1}
	if offset < 0 || offset > len(d.text) {
		return ctx
	}

	line := d.positionOf(offset).Line
	lineStart := d.lineStarts[line]
	before := d.text[lineStart:offset]

	ctx.InComment = inComment(before)
	if ctx.InComment {
		return ctx
	}
	ctx.InString = oddQuotes(before)

	ctx.Prefix, ctx.Receiver, ctx.AfterDot = splitReceiver(string(before))

	if d.root != nil {
		ctx.Call, ctx.ArgIndex = s.enclosingCall(d, offset)
	}
	return ctx
}

// inComment reports whether a hash outside a string opens a comment on this
// line before the caret.
func inComment(before []byte) bool {
	var quote byte
	for i := 0; i < len(before); i++ {
		c := before[i]
		switch {
		case quote != 0:
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '#':
			return true
		}
	}
	return false
}

// oddQuotes reports whether the caret sits inside an unterminated literal on
// this line.
func oddQuotes(before []byte) bool {
	var quote byte
	for i := 0; i < len(before); i++ {
		c := before[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
		}
	}
	return quote != 0
}

// splitReceiver reads backwards from the caret and returns the partial name
// being typed, the dotted receiver it hangs off, and whether a dot was crossed.
//
// It walks balanced brackets so a call in the chain — `fs.path("a/b").rea` —
// stays part of the receiver rather than truncating it.
func splitReceiver(before string) (prefix, receiver string, afterDot bool) {
	i := len(before)

	// The identifier fragment under the caret.
	j := i
	for j > 0 && isIdentByte(before[j-1]) {
		j--
	}
	prefix = before[j:i]
	i = j

	if i == 0 || before[i-1] != '.' {
		return prefix, "", false
	}
	afterDot = true
	i-- // step over the dot

	end := i
	for i > 0 {
		c := before[i-1]
		switch {
		case c == ')' || c == ']' || c == '}':
			open, ok := matchOpen(before[:i])
			if !ok {
				return prefix, strings.TrimSpace(before[i:end]), true
			}
			i = open
		case c == '"' || c == '\'':
			// A string literal is a receiver: "abc".upper() completes.
			open, ok := matchQuote(before[:i], c)
			if !ok {
				return prefix, strings.TrimSpace(before[i:end]), true
			}
			i = open
		case isIdentByte(c) || c == '.':
			i--
		default:
			return prefix, strings.TrimSpace(before[i:end]), true
		}
	}
	return prefix, strings.TrimSpace(before[:end]), true
}

// matchOpen finds the index of the bracket that opens the closer ending s.
func matchOpen(s string) (int, bool) {
	depth := 0
	for i := len(s) - 1; i >= 0; i-- {
		switch s[i] {
		case ')', ']', '}':
			depth++
		case '(', '[', '{':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// enclosingCall finds the innermost call whose argument list contains the
// caret, and which argument the caret sits in.
func (s *Server) enclosingCall(d *Document, offset int) (*gotreesitter.Node, int) {
	var (
		bestCall *gotreesitter.Node
		bestArgs *gotreesitter.Node
	)
	walk(d.root, func(n *gotreesitter.Node) bool {
		if int(n.StartByte()) > offset || int(n.EndByte()) < offset {
			return false
		}
		if n.Type(s.lang) != ntCall {
			return true
		}
		args := childByField(n, "arguments", 1, s.lang)
		if args == nil {
			return true
		}
		// The caret must be strictly inside the parentheses.
		if offset > int(args.StartByte()) && offset <= int(args.EndByte()) {
			bestCall, bestArgs = n, args
		}
		return true
	})
	if bestCall == nil {
		return nil, -1
	}
	return bestCall, argIndexAt(d, bestArgs, offset)
}

// argIndexAt counts the top-level commas between the opening bracket and the
// caret.
func argIndexAt(d *Document, args *gotreesitter.Node, offset int) int {
	start := int(args.StartByte())
	if start >= offset || start >= len(d.text) {
		return 0
	}
	segment := d.text[start+1 : min(offset, len(d.text))]
	index, depth := 0, 0
	var quote byte
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				index++
			}
		}
	}
	return index
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// resolveOwner maps a receiver expression to the owner whose members it
// exposes.
//
// The core performs one step it can justify from the language alone: it
// follows a bare identifier back to the expression that bound it. Everything
// after that is a question about a dialect, so it goes to the host. A host
// that declines leaves the server offering nothing, which is deliberate — a
// wrong completion list is worse than an absent one.
func (s *Server) resolveOwner(d *Document, receiver string) (string, bool) {
	return s.resolveOwnerDepth(d, receiver, 0)
}

// maxResolveDepth stops a cyclic binding — x = y; y = x — from recursing
// without end.
const maxResolveDepth = 8

func (s *Server) resolveOwnerDepth(d *Document, receiver string, depth int) (string, bool) {
	receiver = strings.TrimSpace(receiver)
	if receiver == "" || depth > maxResolveDepth {
		return "", false
	}

	if s.caps.types != nil {
		owner, ok := s.caps.types.ResolveType(TypeRequest{
			Expr:     receiver,
			Document: d,
			Resolve: func(expr string) (string, bool) {
				return s.resolveOwnerDepth(d, expr, depth+1)
			},
		})
		if ok {
			return owner, true
		}
	}

	// Follow a bare identifier to its most recent binding and ask again.
	if isIdentifier(receiver) {
		if bound, ok := s.bindingExpression(d, receiver); ok && bound != receiver {
			return s.resolveOwnerDepth(d, bound, depth+1)
		}
	}
	return "", false
}

// calleeOf returns the callee text of a call expression, if the expression
// is a call.
func calleeOf(expr string) (string, bool) {
	expr = strings.TrimSpace(expr)
	if !strings.HasSuffix(expr, ")") {
		return "", false
	}
	open, ok := matchOpen(expr)
	if !ok {
		return "", false
	}
	callee := strings.TrimSpace(expr[:open])
	if callee == "" {
		return "", false
	}
	return callee, true
}

// splitLast splits "fs.path" into "fs" and "path".
func splitLast(dotted string) (owner, member string, ok bool) {
	dot := strings.LastIndex(dotted, ".")
	if dot <= 0 || dot == len(dotted)-1 {
		return "", "", false
	}
	return dotted[:dot], dotted[dot+1:], true
}

func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return s[0] < '0' || s[0] > '9'
}

// bindingExpression finds the right-hand side of the last assignment to name.
func (s *Server) bindingExpression(d *Document, name string) (string, bool) {
	if d.root == nil {
		return "", false
	}
	var found string
	walk(d.root, func(n *gotreesitter.Node) bool {
		if n.Type(s.lang) != ntAssignment {
			return true
		}
		target := childByField(n, "left", 0, s.lang)
		if target == nil || target.Type(s.lang) != ntIdentifier || d.nodeText(target) != name {
			return true
		}
		if value := childByField(n, "right", 1, s.lang); value != nil {
			found = d.nodeText(value)
		}
		return true
	})
	return found, found != ""
}

// matchQuote finds the index of the quote that opens the literal ending s.
func matchQuote(s string, quote byte) (int, bool) {
	for i := len(s) - 2; i >= 0; i-- {
		if s[i] != quote {
			continue
		}
		// A quote preceded by a backslash is escaped, not the opener.
		backslashes := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return i, true
		}
	}
	return 0, false
}
