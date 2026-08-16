// Package bazel is the Bazel dialect of Starlark, for BUILD, BUILD.bazel,
// WORKSPACE, MODULE.bazel, and .bzl files.
//
// Unlike the specification host in the root package, this one cannot
// introspect its own runtime: Bazel is not a Go library, so there is nothing to
// call AttrNames on. The names below are therefore curated from Bazel's
// published API reference, and the set was checked against real code — every
// name a scan of bazelbuild/rules_go and bazelbuild/bazel-skylib actually
// referenced is present.
//
// That makes this list maintainable rather than authoritative, and it will lag
// Bazel releases. Extending it is deliberately trivial: append to the tables
// below, or compose your own host over this one with starlsp.Hosts.
package bazel

import (
	"sort"
	"sync"

	"go.starlark.net/syntax"

	"github.com/M31-Labs/starlsp"
)

// Host is the Bazel dialect.
type Host struct {
	once    sync.Once
	globals []starlsp.Symbol
	members map[string][]starlsp.Symbol
}

// New returns the Bazel host. Compose it over the specification host:
//
//	starlsp.Hosts(starlsp.NewVanilla(), bazel.New())
func New() *Host { return &Host{} }

func (h *Host) Name() string { return "bazel" }

// FileOptions gives Bazel's dialect. Bazel does not permit top-level control
// flow in BUILD files, and does not enable the set builtin.
func (h *Host) FileOptions() *syntax.FileOptions {
	return &syntax.FileOptions{}
}

func (h *Host) Globals() []starlsp.Symbol {
	h.build()
	return h.globals
}

func (h *Host) Members(owner string) []starlsp.Symbol {
	h.build()
	return h.members[owner]
}

// ResolveType maps a namespace name to itself, so `attr.` and `native.`
// complete. Bazel values are opaque to this host, so nothing else resolves.
func (h *Host) ResolveType(req starlsp.TypeRequest) (string, bool) {
	h.build()
	if _, ok := h.members[req.Expr]; ok {
		return req.Expr, true
	}
	return "", false
}

func (h *Host) build() {
	h.once.Do(func() {
		h.members = map[string][]starlsp.Symbol{
			"attr":            symbols("attr", attrMembers),
			"native":          symbols("native", nativeMembers),
			"config_common":   symbols("config_common", []entry{{"toolchain_type", "toolchain_type(name, mandatory=True)", "ToolchainType", "Declare a toolchain dependency."}}),
			"platform_common": symbols("platform_common", []entry{{"TemplateVariableInfo", "TemplateVariableInfo(vars)", "Provider", "Provider for make-variable substitutions."}, {"ToolchainInfo", "ToolchainInfo(**kwargs)", "Provider", "Provider for toolchain data."}}),
			"config": symbols("config", []entry{
				{"bool", "config.bool(flag=False)", "build_setting", "A boolean build setting."},
				{"int", "config.int(flag=False)", "build_setting", "An integer build setting."},
				{"string", "config.string(flag=False)", "build_setting", "A string build setting."},
				{"string_list", "config.string_list(flag=False, repeatable=False)", "build_setting", "A string-list build setting."},
				{"exec", "config.exec(exec_group=None)", "transition", "Transition to the execution configuration."},
				{"target", "config.target()", "transition", "Transition to the target configuration."},
				{"none", "config.none()", "transition", "Do not transition."},
			}),
			"json":  symbols("json", []entry{{"encode", "json.encode(x)", "string", "Encode a value as JSON."}, {"decode", "json.decode(x, default)", "any", "Decode a JSON string."}, {"indent", "json.indent(s, prefix='', indent='\\t')", "string", "Re-indent a JSON string."}, {"encode_indent", "json.encode_indent(x, prefix='', indent='\\t')", "string", "Encode and indent."}}),
			"proto": symbols("proto", []entry{{"encode_text", "proto.encode_text(x)", "string", "Encode a struct as text-format protobuf."}}),
		}

		for _, e := range globalEntries {
			h.globals = append(h.globals, starlsp.Symbol{
				Name: e.name, Kind: starlsp.KindFunction,
				Signature: e.signature, Returns: e.returns, Doc: e.doc,
			})
		}
		for _, name := range namespaceNames {
			h.globals = append(h.globals, starlsp.Symbol{
				Name: name, Kind: starlsp.KindNamespace, Doc: namespaceDocs[name],
			})
		}
		for _, name := range providerNames {
			h.globals = append(h.globals, starlsp.Symbol{
				Name: name, Kind: starlsp.KindType,
				Doc: "Bazel provider. See the Bazel build encyclopedia.",
			})
		}
		sort.Slice(h.globals, func(i, j int) bool { return h.globals[i].Name < h.globals[j].Name })
	})
}

type entry struct{ name, signature, returns, doc string }

func symbols(owner string, entries []entry) []starlsp.Symbol {
	out := make([]starlsp.Symbol, 0, len(entries))
	for _, e := range entries {
		out = append(out, starlsp.Symbol{
			Name: e.name, Owner: owner, Kind: starlsp.KindFunction,
			Signature: e.signature, Returns: e.returns, Doc: e.doc,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var namespaceNames = []string{"attr", "native", "config", "config_common", "platform_common", "json", "proto", "apple_common", "cc_common", "java_common", "testing", "coverage_common"}

var namespaceDocs = map[string]string{
	"attr":            "Attribute schema constructors for rule and aspect definitions.",
	"native":          "Native build rules and helpers, callable from a macro in a BUILD file.",
	"config":          "Build-setting and transition constructors.",
	"config_common":   "Configuration helpers.",
	"platform_common": "Platform and toolchain providers.",
	"json":            "JSON encoding and decoding.",
	"proto":           "Protocol buffer text encoding.",
	"apple_common":    "Apple platform helpers.",
	"cc_common":       "C++ rule helpers.",
	"java_common":     "Java rule helpers.",
	"testing":         "Test helpers for analysis tests.",
	"coverage_common": "Coverage instrumentation helpers.",
}

// attrMembers is the highest-value table in this package. A scan of real rule
// code found `attr.` referenced 378 times, more than any other name.
var attrMembers = []entry{
	{"bool", "attr.bool(default=False, doc='', mandatory=False)", "Attribute", "A boolean attribute."},
	{"int", "attr.int(default=0, doc='', mandatory=False, values=[])", "Attribute", "An integer attribute."},
	{"int_list", "attr.int_list(mandatory=False, allow_empty=True, default=[])", "Attribute", "A list-of-integers attribute."},
	{"label", "attr.label(default=None, doc='', executable=False, allow_files=None, allow_single_file=None, mandatory=False, providers=[], cfg=None, aspects=[])", "Attribute", "A label attribute, creating a dependency."},
	{"label_keyed_string_dict", "attr.label_keyed_string_dict(allow_files=False, default={}, ...)", "Attribute", "A label-keyed string dictionary attribute."},
	{"label_list", "attr.label_list(allow_empty=True, default=[], doc='', allow_files=None, mandatory=False, providers=[], cfg=None, aspects=[])", "Attribute", "A list-of-labels attribute."},
	{"output", "attr.output(doc='', mandatory=False)", "Attribute", "An output label attribute."},
	{"output_list", "attr.output_list(allow_empty=True, mandatory=False)", "Attribute", "A list-of-output-labels attribute."},
	{"string", "attr.string(default='', doc='', mandatory=False, values=[])", "Attribute", "A string attribute."},
	{"string_dict", "attr.string_dict(allow_empty=True, default={}, mandatory=False)", "Attribute", "A string-to-string dictionary attribute."},
	{"string_keyed_label_dict", "attr.string_keyed_label_dict(allow_empty=True, default={}, ...)", "Attribute", "A string-keyed label dictionary attribute."},
	{"string_list", "attr.string_list(mandatory=False, allow_empty=True, default=[])", "Attribute", "A list-of-strings attribute."},
	{"string_list_dict", "attr.string_list_dict(allow_empty=True, default={}, mandatory=False)", "Attribute", "A string-to-string-list dictionary attribute."},
}

var nativeMembers = []entry{
	{"glob", "native.glob(include=[], exclude=[], exclude_directories=1, allow_empty=True)", "list", "Match source files by pattern."},
	{"package_name", "native.package_name()", "string", "The name of the package being evaluated."},
	{"package_relative_label", "native.package_relative_label(input)", "Label", "Resolve a label relative to this package."},
	{"repository_name", "native.repository_name()", "string", "The name of the repository, prefixed with @."},
	{"existing_rule", "native.existing_rule(name)", "dict", "Attributes of an already-defined rule in this package."},
	{"existing_rules", "native.existing_rules()", "dict", "Every already-defined rule in this package."},
	{"exports_files", "native.exports_files(srcs, visibility=None, licenses=None)", "None", "Export files to other packages."},
	{"package_group", "native.package_group(name, packages=[], includes=[])", "None", "Define a set of packages."},
	{"subpackages", "native.subpackages(include, exclude=[], allow_empty=False)", "list", "List direct subpackages."},
}

// globalEntries are the names callable with no qualifier.
var globalEntries = []entry{
	{"rule", "rule(implementation, test=False, attrs={}, outputs=None, executable=False, ...)", "callable", "Define a new build rule."},
	{"aspect", "aspect(implementation, attr_aspects=[], attrs={}, ...)", "Aspect", "Define an aspect, which propagates along dependencies."},
	{"provider", "provider(doc='', fields=None, init=None)", "Provider", "Define a provider, the unit of information rules exchange."},
	{"repository_rule", "repository_rule(implementation, attrs=None, local=False, environ=[], ...)", "callable", "Define an external repository rule."},
	{"module_extension", "module_extension(implementation, tag_classes={}, doc='', ...)", "extension", "Define a module extension for Bzlmod."},
	{"tag_class", "tag_class(attrs={}, doc='')", "tag_class", "Define a tag class for a module extension."},
	{"depset", "depset(direct=None, order='default', transitive=None)", "depset", "A specialised set for efficient transitive collection."},
	{"select", "select(x, no_match_error='')", "select", "Choose an attribute value by configuration."},
	{"struct", "struct(**kwargs)", "struct", "An immutable record with named fields."},
	{"Label", "Label(label_string)", "Label", "Convert a label string into a Label object."},
	{"glob", "glob(include=[], exclude=[], exclude_directories=1, allow_empty=True)", "list", "Match source files by pattern."},
	{"package", "package(default_visibility=None, default_deprecation=None, features=[], ...)", "None", "Declare metadata for every rule in this package."},
	{"package_name", "package_name()", "string", "The name of the package being evaluated."},
	{"package_group", "package_group(name, packages=[], includes=[])", "None", "Define a set of packages."},
	{"repository_name", "repository_name()", "string", "The name of the repository, prefixed with @."},
	{"licenses", "licenses(license_types)", "None", "Declare the license types of this package."},
	{"exports_files", "exports_files(srcs, visibility=None, licenses=None)", "None", "Export files to other packages."},
	{"filegroup", "filegroup(name, srcs=[], data=[], visibility=None)", "None", "Group files under one label."},
	{"genrule", "genrule(name, srcs=[], outs=[], cmd='', tools=[], ...)", "None", "Generate files with a shell command."},
	{"alias", "alias(name, actual, visibility=None)", "None", "Another name for a target."},
	{"test_suite", "test_suite(name, tests=[], tags=[])", "None", "Group tests into one target."},
	{"config_setting", "config_setting(name, values={}, flag_values={}, constraint_values=[])", "None", "Match a configuration state, for use in select()."},
	{"constraint_setting", "constraint_setting(name, default_constraint_value=None)", "None", "Declare a platform constraint dimension."},
	{"constraint_value", "constraint_value(name, constraint_setting)", "None", "Declare a value of a platform constraint."},
	{"platform", "platform(name, constraint_values=[], parents=[])", "None", "Declare a platform."},
	{"toolchain", "toolchain(name, toolchain_type, toolchain, exec_compatible_with=[], target_compatible_with=[])", "None", "Declare a toolchain."},
	{"toolchain_type", "toolchain_type(name)", "None", "Declare a toolchain type."},
	{"transition", "transition(implementation, inputs, outputs)", "transition", "Define a configuration transition."},
	{"configuration_field", "configuration_field(fragment, name)", "LateBoundDefault", "Reference a late-bound configuration value."},
	{"workspace", "workspace(name)", "None", "Declare the workspace name, in a WORKSPACE file."},
	{"register_toolchains", "register_toolchains(*toolchain_labels)", "None", "Register toolchains for resolution."},
	{"register_execution_platforms", "register_execution_platforms(*platform_labels)", "None", "Register execution platforms."},
	{"visibility", "visibility(value)", "None", "Set the visibility of a .bzl file's symbols."},
	{"bind", "bind(name, actual=None)", "None", "Alias a target in the //external package. Deprecated."},
	{"exec_group", "exec_group(toolchains=[], exec_compatible_with=[])", "exec_group", "Declare an execution group for a rule."},
	{"local_repository", "local_repository(name, path, repo_mapping={})", "None", "Bind a local directory as an external repository."},
	{"new_local_repository", "new_local_repository(name, path, build_file='', build_file_content='')", "None", "Bind a local directory as a repository, supplying a BUILD file."},
	{"label_flag", "label_flag(name, build_setting_default, visibility=None)", "None", "A build setting whose value is a label."},
	{"label_setting", "label_setting(name, build_setting_default, visibility=None)", "None", "A read-only build setting whose value is a label."},
	{"analysis_test_transition", "analysis_test_transition(settings)", "transition", "A configuration transition for analysis tests."},
	{"existing_rule", "existing_rule(name)", "dict", "Attributes of an already-defined rule."},
	{"existing_rules", "existing_rules()", "dict", "Every already-defined rule in this package."},
	{"subpackages", "subpackages(include, exclude=[], allow_empty=False)", "list", "List direct subpackages."},
	{"module", "module(name='', version='', compatibility_level=0, ...)", "None", "Declare a Bazel module, in MODULE.bazel."},
	{"bazel_dep", "bazel_dep(name, version='', repo_name='', dev_dependency=False)", "None", "Declare a dependency on another Bazel module."},
	{"use_extension", "use_extension(extension_bzl_file, extension_name, dev_dependency=False)", "extension_proxy", "Use a module extension."},
	{"use_repo", "use_repo(extension_proxy, *args, **kwargs)", "None", "Import repositories from a module extension."},
	{"single_version_override", "single_version_override(module_name, version='', registry='', patches=[], ...)", "None", "Pin a module to one version."},
	{"local_path_override", "local_path_override(module_name, path)", "None", "Override a module with a local directory."},
	{"git_override", "git_override(module_name, remote, commit='', patches=[], ...)", "None", "Override a module with a git repository."},
	{"archive_override", "archive_override(module_name, urls, integrity='', patches=[], ...)", "None", "Override a module with an archive."},
}

// providerNames are the built-in providers rule implementations return and
// consume.
var providerNames = []string{
	"DefaultInfo", "OutputGroupInfo", "RunEnvironmentInfo", "InstrumentedFilesInfo",
	"CcInfo", "CcToolchainConfigInfo", "DebugPackageInfo",
	"JavaInfo", "JavaPluginInfo", "JavaToolchainInfo", "JavaRuntimeInfo",
	"PyInfo", "PyRuntimeInfo", "ProtoInfo", "AnalysisTestResultInfo",
	"ToolchainTypeInfo", "TemplateVariableInfo", "PackageSpecificationInfo",
	"AnalysisFailureInfo", "AnalysisTestResultInfo", "DefaultInfo",
	"DefaultInfoWithFiles", "Actions",
}

var (
	_ starlsp.Host           = (*Host)(nil)
	_ starlsp.MemberProvider = (*Host)(nil)
	_ starlsp.TypeResolver   = (*Host)(nil)
	_ starlsp.DialectOptions = (*Host)(nil)
)
