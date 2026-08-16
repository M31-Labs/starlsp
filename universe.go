package starlsp

import (
	"sort"
	"strings"
	"sync"

	"go.starlark.net/starlark"
)

// Vanilla is the Host for Starlark as the specification defines it, with no
// dialect extensions.
//
// Its name set is introspected rather than written down. The globals come from
// starlark.Universe, and the methods on strings, lists, dicts, sets, and bytes
// come from calling AttrNames on a zero value of each type. A build of
// go.starlark.net that adds a method therefore gains completion for it with no
// change here.
//
// The documentation below is written by hand, because a Starlark builtin
// carries neither its parameter names nor its description. That is acceptable
// for exactly one reason: the Starlark specification is frozen. A hand-written
// table tracking a moving API rots; a hand-written table tracking a
// specification does not.
type Vanilla struct {
	once    sync.Once
	globals []Symbol
	members map[string][]Symbol
}

// NewVanilla returns the specification-only host.
func NewVanilla() *Vanilla { return &Vanilla{} }

func (v *Vanilla) Name() string { return "starlark" }

func (v *Vanilla) Globals() []Symbol {
	v.build()
	return v.globals
}

func (v *Vanilla) Members(owner string) []Symbol {
	v.build()
	return v.members[owner]
}

// ResolveType infers the type of a literal receiver, so that completion works
// after a string, list, dict, or set written inline.
func (v *Vanilla) ResolveType(req TypeRequest) (string, bool) {
	expr := strings.TrimSpace(req.Expr)
	if expr == "" {
		return "", false
	}
	switch {
	case isStringLiteral(expr):
		return "string", true
	case strings.HasPrefix(expr, "[") && strings.HasSuffix(expr, "]"):
		return "list", true
	case strings.HasPrefix(expr, "{") && strings.HasSuffix(expr, "}"):
		return "dict", true
	}
	// A call to a constructor names its own result: list(...), dict(...).
	if callee, ok := calleeOf(expr); ok {
		switch callee {
		case "list", "dict", "set", "bytes", "str", "tuple", "range":
			return callee, true
		}
	}
	return "", false
}

func isStringLiteral(expr string) bool {
	if len(expr) < 2 {
		return false
	}
	first, last := expr[0], expr[len(expr)-1]
	return (first == '"' || first == '\'') && first == last
}

// build introspects the runtime once.
func (v *Vanilla) build() {
	v.once.Do(func() {
		v.members = make(map[string][]Symbol)

		// ---- globals, from the specification's universe ----
		names := make([]string, 0, len(starlark.Universe))
		for name := range starlark.Universe {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			value := starlark.Universe[name]
			sym := Symbol{Name: name, Kind: universeKind(value)}
			if doc, ok := universeDocs[name]; ok {
				sym.Signature, sym.Returns, sym.Doc = doc.signature, doc.returns, doc.doc
			}
			v.globals = append(v.globals, sym)
		}

		// Starlark's keywords are part of the language, not the universe, so
		// they are listed rather than introspected. The list is closed by the
		// grammar.
		for _, kw := range starlarkKeywords {
			v.globals = append(v.globals, Symbol{Name: kw, Kind: KindKeyword, Detail: "keyword"})
		}

		// ---- methods, from zero values of each builtin type ----
		for owner, probe := range map[string]starlark.Value{
			"string": starlark.String(""),
			"list":   starlark.NewList(nil),
			"dict":   starlark.NewDict(0),
			"set":    starlark.NewSet(0),
			"bytes":  starlark.Bytes(""),
		} {
			v.members[owner] = methodSymbols(owner, probe)
		}

		sort.Slice(v.globals, func(i, j int) bool { return v.globals[i].Name < v.globals[j].Name })
	})
}

// methodSymbols reads the methods off a value.
func methodSymbols(owner string, value starlark.Value) []Symbol {
	attrs, ok := value.(starlark.HasAttrs)
	if !ok {
		return nil
	}
	names := attrs.AttrNames()
	sort.Strings(names)

	out := make([]Symbol, 0, len(names))
	for _, name := range names {
		sym := Symbol{Name: name, Owner: owner, Kind: KindMethod}
		if doc, ok := methodDocs[owner+"."+name]; ok {
			sym.Signature, sym.Returns, sym.Doc = doc.signature, doc.returns, doc.doc
		}
		out = append(out, sym)
	}
	return out
}

// universeKind classifies a universe entry by what it actually is.
func universeKind(value starlark.Value) Kind {
	switch v := value.(type) {
	case *starlark.Builtin:
		// A builtin whose name is a type is a constructor.
		switch v.Name() {
		case "bool", "bytes", "dict", "float", "int", "list", "range", "set", "str", "tuple":
			return KindType
		}
		return KindFunction
	case starlark.NoneType, starlark.Bool:
		return KindConstant
	default:
		return KindVariable
	}
}

// starlarkKeywords is closed by the grammar.
var starlarkKeywords = []string{
	"and", "break", "continue", "def", "elif", "else", "for", "if",
	"in", "lambda", "load", "not", "or", "pass", "return", "while",
}

type specDoc struct{ signature, returns, doc string }

// universeDocs documents the specification's built-in constants and functions.
// Source: https://github.com/bazelbuild/starlark/blob/master/spec.md
var universeDocs = map[string]specDoc{
	"None":  {"None", "NoneType", "The absent value."},
	"True":  {"True", "bool", "The true value."},
	"False": {"False", "bool", "The false value."},

	"abs":       {"abs(x)", "int or float", "Return the absolute value of a number."},
	"all":       {"all(x)", "bool", "Report whether every element of the iterable is true."},
	"any":       {"any(x)", "bool", "Report whether any element of the iterable is true."},
	"bool":      {"bool(x=False)", "bool", "Convert a value to a boolean by its truth."},
	"bytes":     {"bytes(x)", "bytes", "Convert a string or iterable of ints to bytes."},
	"chr":       {"chr(i)", "string", "Return the string of the single code point i."},
	"dict":      {"dict(pairs=[], **kwargs)", "dict", "Create a dictionary from pairs and keyword arguments."},
	"dir":       {"dir(x)", "list", "Return the sorted names of the value's attributes."},
	"enumerate": {"enumerate(x, start=0)", "list", "Pair each element of the iterable with its index."},
	"fail":      {"fail(*args, sep=\" \")", "never returns", "Abort execution with an error message."},
	"float":     {"float(x=0.0)", "float", "Convert a number or string to a float."},
	"getattr":   {"getattr(x, name, default)", "any", "Return a named attribute, or default when absent."},
	"hasattr":   {"hasattr(x, name)", "bool", "Report whether a value has the named attribute."},
	"hash":      {"hash(x)", "int", "Return a hash of a string or bytes value."},
	"int":       {"int(x=0, base)", "int", "Convert a number or string to an integer."},
	"len":       {"len(x)", "int", "Return the number of elements in a sequence."},
	"list":      {"list(x=[])", "list", "Create a new mutable list from an iterable."},
	"max":       {"max(x, *args, key=None)", "any", "Return the greatest value."},
	"min":       {"min(x, *args, key=None)", "any", "Return the smallest value."},
	"ord":       {"ord(s)", "int", "Return the code point of a single-character string."},
	"print":     {"print(*args, sep=\" \")", "None", "Write values to the host's log."},
	"range":     {"range(start, stop, step=1)", "range", "Return an immutable sequence of integers."},
	"repr":      {"repr(x)", "string", "Return a printable representation of a value."},
	"reversed":  {"reversed(x)", "list", "Return a new list with the elements in reverse order."},
	"set":       {"set(x=[])", "set", "Create a set from an iterable. Not enabled in every dialect."},
	"sorted":    {"sorted(x, key=None, reverse=False)", "list", "Return a new sorted list."},
	"str":       {"str(x)", "string", "Convert a value to a string."},
	"tuple":     {"tuple(x=())", "tuple", "Create a tuple from an iterable."},
	"type":      {"type(x)", "string", "Return the name of a value's type."},
	"zip":       {"zip(*args)", "list", "Return a list of tuples drawn from each iterable in turn."},
}

// methodDocs documents the methods on the specification's builtin types.
var methodDocs = map[string]specDoc{
	// string
	"string.capitalize":     {"capitalize()", "string", "Return a copy with the first code point upper case and the rest lower."},
	"string.codepoint_ords": {"codepoint_ords()", "iterable", "Return an iterable of the code points, as integers."},
	"string.codepoints":     {"codepoints()", "iterable", "Return an iterable of the code points, as strings."},
	"string.count":          {"count(sub, start=0, end=len)", "int", "Count non-overlapping occurrences of a substring."},
	"string.elem_ords":      {"elem_ords()", "iterable", "Return an iterable of the bytes, as integers."},
	"string.elems":          {"elems()", "iterable", "Return an iterable of the bytes, as strings."},
	"string.endswith":       {"endswith(suffix, start=0, end=len)", "bool", "Report whether the string ends with a suffix."},
	"string.find":           {"find(sub, start=0, end=len)", "int", "Return the first index of a substring, or -1."},
	"string.format":         {"format(*args, **kwargs)", "string", "Substitute {} placeholders with arguments."},
	"string.index":          {"index(sub, start=0, end=len)", "int", "Return the first index of a substring, or fail."},
	"string.isalnum":        {"isalnum()", "bool", "Report whether the string is non-empty and all alphanumeric."},
	"string.isalpha":        {"isalpha()", "bool", "Report whether the string is non-empty and all alphabetic."},
	"string.isdigit":        {"isdigit()", "bool", "Report whether the string is non-empty and all digits."},
	"string.islower":        {"islower()", "bool", "Report whether the cased code points are lower case."},
	"string.isspace":        {"isspace()", "bool", "Report whether the string is non-empty and all whitespace."},
	"string.istitle":        {"istitle()", "bool", "Report whether the string is title cased."},
	"string.isupper":        {"isupper()", "bool", "Report whether the cased code points are upper case."},
	"string.join":           {"join(iterable)", "string", "Join the elements of an iterable with this separator."},
	"string.lower":          {"lower()", "string", "Return a lower case copy."},
	"string.lstrip":         {"lstrip(cutset=\" \")", "string", "Strip leading characters."},
	"string.partition":      {"partition(sep)", "tuple", "Split once at the first separator into three parts."},
	"string.removeprefix":   {"removeprefix(prefix)", "string", "Remove a prefix when present."},
	"string.removesuffix":   {"removesuffix(suffix)", "string", "Remove a suffix when present."},
	"string.replace":        {"replace(old, new, count=-1)", "string", "Replace occurrences of a substring."},
	"string.rfind":          {"rfind(sub, start=0, end=len)", "int", "Return the last index of a substring, or -1."},
	"string.rindex":         {"rindex(sub, start=0, end=len)", "int", "Return the last index of a substring, or fail."},
	"string.rpartition":     {"rpartition(sep)", "tuple", "Split once at the last separator into three parts."},
	"string.rsplit":         {"rsplit(sep=None, maxsplit=-1)", "list", "Split from the right."},
	"string.rstrip":         {"rstrip(cutset=\" \")", "string", "Strip trailing characters."},
	"string.split":          {"split(sep=None, maxsplit=-1)", "list", "Split on a separator."},
	"string.splitlines":     {"splitlines(keepends=False)", "list", "Split at line boundaries."},
	"string.startswith":     {"startswith(prefix, start=0, end=len)", "bool", "Report whether the string starts with a prefix."},
	"string.strip":          {"strip(cutset=\" \")", "string", "Strip leading and trailing characters."},
	"string.title":          {"title()", "string", "Return a title cased copy."},
	"string.upper":          {"upper()", "string", "Return an upper case copy."},

	// list
	"list.append": {"append(x)", "None", "Add an element to the end."},
	"list.clear":  {"clear()", "None", "Remove every element."},
	"list.extend": {"extend(iterable)", "None", "Add every element of an iterable to the end."},
	"list.index":  {"index(x, start=0, end=len)", "int", "Return the first index of a value, or fail."},
	"list.insert": {"insert(i, x)", "None", "Insert an element at an index."},
	"list.pop":    {"pop(i=-1)", "any", "Remove and return the element at an index."},
	"list.remove": {"remove(x)", "None", "Remove the first occurrence of a value."},

	// dict
	"dict.clear":      {"clear()", "None", "Remove every entry."},
	"dict.get":        {"get(key, default=None)", "any", "Return the value for a key, or a default."},
	"dict.items":      {"items()", "list", "Return the key and value pairs."},
	"dict.keys":       {"keys()", "list", "Return the keys."},
	"dict.pop":        {"pop(key, default)", "any", "Remove a key and return its value."},
	"dict.popitem":    {"popitem()", "tuple", "Remove and return one key and value pair."},
	"dict.setdefault": {"setdefault(key, default=None)", "any", "Return a value, inserting a default when absent."},
	"dict.update":     {"update(pairs=[], **kwargs)", "None", "Add entries from pairs and keyword arguments."},
	"dict.values":     {"values()", "list", "Return the values."},

	// set
	"set.add":                  {"add(x)", "None", "Add an element."},
	"set.clear":                {"clear()", "None", "Remove every element."},
	"set.difference":           {"difference(*others)", "set", "Return the elements not in the others."},
	"set.discard":              {"discard(x)", "None", "Remove an element when present."},
	"set.intersection":         {"intersection(*others)", "set", "Return the elements common to all."},
	"set.issubset":             {"issubset(other)", "bool", "Report whether every element is in the other."},
	"set.issuperset":           {"issuperset(other)", "bool", "Report whether the other's elements are all present."},
	"set.pop":                  {"pop()", "any", "Remove and return one element."},
	"set.remove":               {"remove(x)", "None", "Remove an element, or fail when absent."},
	"set.symmetric_difference": {"symmetric_difference(other)", "set", "Return the elements in exactly one of the two."},
	"set.union":                {"union(*others)", "set", "Return the elements in any of them."},
	"set.update":               {"update(*others)", "None", "Add the elements of the others."},

	// bytes
	"bytes.elems": {"elems()", "iterable", "Return an iterable of the bytes, as integers."},
}

var (
	_ Host           = (*Vanilla)(nil)
	_ MemberProvider = (*Vanilla)(nil)
	_ TypeResolver   = (*Vanilla)(nil)
)
