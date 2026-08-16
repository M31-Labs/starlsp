package starlsp

import "strings"

// Kind classifies a name for the editor.
type Kind int

// The kinds a host can produce. They map onto LSP completion item kinds and
// onto semantic token types.
const (
	KindUnknown Kind = iota
	KindFunction
	KindMethod
	KindNamespace // a module or package: fs, http
	KindType      // a constructible type: Path, Response
	KindProperty  // an attribute read without a call
	KindVariable
	KindConstant
	KindKeyword
	KindParameter
)

// A Symbol is one completable, hoverable name.
//
// A host builds these from whatever it actually knows. Only Name is required;
// everything else improves what the editor shows and none of it is guessed by
// the server.
type Symbol struct {
	// Name is the identifier as written in source: "read_text".
	Name string

	// Owner is the namespace or type that holds it, empty for a global.
	Owner string

	// Kind classifies it for completion and highlighting.
	Kind Kind

	// Signature is the full call form when known: "fs.path(p)". The server
	// parses parameters out of it for signature help, so the parentheses
	// matter.
	Signature string

	// Returns is the result type, shown beside the name in completion.
	Returns string

	// Doc is markdown prose shown on hover.
	Doc string

	// Deprecated marks a name the editor should strike through.
	Deprecated bool

	// SortText overrides completion ordering. Leave empty for the default,
	// which sorts host globals after file-local names.
	SortText string

	// Detail overrides the right-hand text in the completion list. Leave empty
	// to let the server compose it from Signature and Returns.
	Detail string
}

// Qualified renders the dotted path of a symbol.
func (s Symbol) Qualified() string {
	if s.Owner == "" {
		return s.Name
	}
	return s.Owner + "." + s.Name
}

// Params splits the parameter list out of Signature. It returns nil when the
// symbol has no signature or is not a call.
func (s Symbol) Params() []string {
	open := strings.Index(s.Signature, "(")
	if open < 0 || !strings.HasSuffix(strings.TrimSpace(s.Signature), ")") {
		return nil
	}
	inner := strings.TrimSpace(s.Signature)
	return splitTopLevel(inner[open+1 : len(inner)-1])
}

// splitTopLevel splits on commas that are not nested inside brackets or
// quotes, so a default value containing a comma stays with its parameter.
func splitTopLevel(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	var (
		out   []string
		depth int
		quote rune
		cur   strings.Builder
	)
	flush := func() {
		if p := strings.TrimSpace(cur.String()); p != "" {
			out = append(out, p)
		}
		cur.Reset()
	}
	for _, r := range text {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == '(' || r == '[' || r == '{':
			depth++
			cur.WriteRune(r)
		case r == ')' || r == ']' || r == '}':
			depth--
			cur.WriteRune(r)
		case r == ',' && depth == 0:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// completionKind maps a symbol kind onto the LSP completion item kind.
func (k Kind) completionKind() CompletionItemKind {
	switch k {
	case KindFunction:
		return kindFunction
	case KindMethod:
		return kindMethod
	case KindNamespace:
		return kindModule
	case KindType:
		return kindClass
	case KindProperty:
		return kindProperty
	case KindVariable:
		return kindVariable
	case KindConstant:
		return kindConstant
	case KindKeyword:
		return kindKeyword
	case KindParameter:
		return kindVariable
	default:
		return kindText
	}
}

// tokenType maps a symbol kind onto this server's semantic token legend.
func (k Kind) tokenType() int {
	switch k {
	case KindFunction:
		return tokFunction
	case KindMethod:
		return tokMethod
	case KindNamespace:
		return tokNamespace
	case KindType:
		return tokType
	case KindProperty:
		return tokProperty
	case KindConstant:
		return tokVariable
	case KindKeyword:
		return tokKeyword
	case KindParameter:
		return tokParameter
	default:
		return tokVariable
	}
}
