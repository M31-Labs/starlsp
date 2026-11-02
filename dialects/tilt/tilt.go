// Package tilt is the Tilt dialect of Starlark, for Tiltfiles.
//
// Tilt differs from the specification in a way that matters more than its
// builtin list: it permits if, for, and while at the top level. Real Tiltfiles
// in tilt-dev/tilt use top-level if, so a server enforcing the specification
// reports a syntax error on files Tilt itself accepts. FileOptions below is
// the fix, and it is the reason this package exists as much as the API is.
//
// The names are curated from Tilt's API reference. Like the Bazel dialect,
// this cannot introspect a runtime, so it will lag Tilt releases; extending it
// means appending to the table below.
package tilt

import (
	"sort"
	"sync"

	"go.starlark.net/syntax"

	"github.com/M31-Labs/starlsp"
)

// Host is the Tilt dialect.
type Host struct {
	once    sync.Once
	globals []starlsp.Symbol
}

// New returns the Tilt host. Compose it over the specification host:
//
//	starlsp.Hosts(starlsp.NewVanilla(), tilt.New())
func New() *Host { return &Host{} }

func (h *Host) Name() string { return "tilt" }

// FileOptions permits the top-level control flow Tiltfiles actually use.
//
// It also accepts the set builtin. This host cannot tell whether the Tilt that
// runs a Tiltfile has sets, and a server that reports an error on a file Tilt
// accepts is worse than one that misses an error Tilt would give.
func (h *Host) FileOptions() *syntax.FileOptions {
	return &syntax.FileOptions{
		TopLevelControl: true,
		While:           true,
		GlobalReassign:  true,
		Set:             true,
	}
}

func (h *Host) Globals() []starlsp.Symbol {
	h.once.Do(func() {
		for _, e := range entries {
			h.globals = append(h.globals, starlsp.Symbol{
				Name: e.name, Kind: starlsp.KindFunction,
				Signature: e.signature, Returns: e.returns, Doc: e.doc,
			})
		}
		sort.Slice(h.globals, func(i, j int) bool { return h.globals[i].Name < h.globals[j].Name })
	})
	return h.globals
}

type entry struct{ name, signature, returns, doc string }

var entries = []entry{
	{"docker_build", "docker_build(ref, context, build_args={}, dockerfile='Dockerfile', live_update=[], target='', ...)", "None", "Build a Docker image from a context directory."},
	{"custom_build", "custom_build(ref, command, deps, tag='', live_update=[], ...)", "None", "Build an image with an arbitrary command."},
	{"k8s_yaml", "k8s_yaml(yaml, allow_duplicates=False)", "None", "Register Kubernetes manifests with Tilt."},
	{"k8s_resource", "k8s_resource(workload='', new_name='', port_forwards=[], resource_deps=[], labels=[], ...)", "None", "Configure a Kubernetes workload."},
	{"k8s_custom_deploy", "k8s_custom_deploy(name, apply_cmd, delete_cmd, deps, image_deps=[], ...)", "None", "Deploy with custom apply and delete commands."},
	{"k8s_kind", "k8s_kind(kind, api_version=None, image_json_path=[], pod_readiness='')", "None", "Teach Tilt about a custom resource kind."},
	{"local_resource", "local_resource(name, cmd, deps=[], trigger_mode=..., resource_deps=[], serve_cmd='', ...)", "None", "Run a command on the host as a resource."},
	{"local", "local(command, quiet=False, command_bat='', echo_off=False, env={}, dir='')", "Blob", "Run a command at Tiltfile execution time and capture its output."},
	{"read_file", "read_file(path, default=None)", "Blob", "Read a file from disk."},
	{"read_yaml", "read_yaml(path, default=None)", "any", "Read and parse a YAML file."},
	{"read_yaml_stream", "read_yaml_stream(path, default=None)", "list", "Read a multi-document YAML file."},
	{"read_json", "read_json(path, default=None)", "any", "Read and parse a JSON file."},
	{"decode_json", "decode_json(json)", "any", "Parse a JSON string."},
	{"encode_json", "encode_json(obj)", "Blob", "Serialise a value as JSON."},
	{"decode_yaml", "decode_yaml(yaml)", "any", "Parse a YAML string."},
	{"decode_yaml_stream", "decode_yaml_stream(yaml)", "list", "Parse a multi-document YAML string."},
	{"encode_yaml", "encode_yaml(obj)", "Blob", "Serialise a value as YAML."},
	{"encode_yaml_stream", "encode_yaml_stream(objs)", "Blob", "Serialise values as a multi-document YAML stream."},
	{"watch_file", "watch_file(path)", "None", "Re-execute the Tiltfile when a path changes."},
	{"include", "include(path)", "None", "Execute another Tiltfile in the current scope."},
	{"load_dynamic", "load_dynamic(path)", "dict", "Load an extension at runtime and return its symbols."},
	{"default_registry", "default_registry(host, host_from_cluster='', single_name='')", "None", "Push images to a registry instead of the local daemon."},
	{"allow_k8s_contexts", "allow_k8s_contexts(contexts)", "None", "Permit Tilt to run against the named contexts."},
	{"k8s_context", "k8s_context()", "string", "The current Kubernetes context."},
	{"k8s_namespace", "k8s_namespace()", "string", "The current Kubernetes namespace."},
	{"blob", "blob(contents)", "Blob", "Wrap a string so Tilt treats it as file contents."},
	{"listdir", "listdir(directory, recursive=False)", "list", "List the files in a directory."},
	{"os", "os", "module", "Operating-system helpers: os.getenv, os.path, os.name."},
	{"config", "config", "module", "Tiltfile configuration: config.define_string_list, config.parse, config.set_enabled_resources."},
	{"sync", "sync(local_path, remote_path)", "LiveUpdateStep", "A live-update step that copies files into a running container."},
	{"run", "run(cmd, trigger=[])", "LiveUpdateStep", "A live-update step that runs a command in a running container."},
	{"restart_container", "restart_container()", "LiveUpdateStep", "A live-update step that restarts the container."},
	{"fall_back_on", "fall_back_on(files)", "LiveUpdateStep", "Fall back to a full image build when these files change."},
	{"port_forward", "port_forward(local_port, container_port=None, name='', link_path='', host='')", "PortForward", "Forward a container port to the host."},
	{"link", "link(url, name='')", "Link", "Attach a link to a resource in the Tilt UI."},
	{"trigger_mode", "trigger_mode(mode)", "None", "Set the default trigger mode for the Tiltfile."},
	{"update_settings", "update_settings(max_parallel_updates=3, k8s_upsert_timeout_secs=30, suppress_unused_image_warnings=None)", "None", "Tune Tilt's update behaviour."},
	{"secret_settings", "secret_settings(disable_scrub=False)", "None", "Control secret scrubbing in logs."},
	{"analytics_settings", "analytics_settings(enable)", "None", "Opt in or out of analytics."},
	{"version_settings", "version_settings(check_updates=True, constraint='')", "None", "Constrain the Tilt version this Tiltfile supports."},
	{"disable_snapshots", "disable_snapshots()", "None", "Disable snapshot creation."},
	{"warn", "warn(msg)", "None", "Print a warning in the Tilt UI."},
	{"fail", "fail(msg)", "None", "Abort Tiltfile execution with a message."},
	{"exit", "exit(code)", "None", "Stop executing the Tiltfile early."},
	{"str_to_json", "str_to_json(s)", "any", "Deprecated. Use decode_json."},
	{"kustomize", "kustomize(pathToDir, kustomize_bin='', flags=[])", "Blob", "Render manifests with kustomize."},
	{"helm", "helm(pathToChartDir, name='', namespace='', values=[], set=[])", "Blob", "Render a Helm chart."},
	{"docker_compose", "docker_compose(configPaths, env_file='', profiles=[], project_name='', wait=False)", "None", "Use a docker-compose file to define resources."},
	{"dc_resource", "dc_resource(name, new_name='', trigger_mode=..., resource_deps=[], labels=[], ...)", "None", "Configure a docker-compose service."},
	{"probe", "probe(initial_delay_secs=0, timeout_secs=1, period_secs=10, success_threshold=1, failure_threshold=3, exec=None, http_get=None, tcp_socket=None)", "Probe", "Define a readiness probe."},
	{"http_get_action", "http_get_action(port, host='', scheme='', path='')", "action", "An HTTP readiness check."},
	{"exec_action", "exec_action(command)", "action", "A command readiness check."},
	{"tcp_socket_action", "tcp_socket_action(port, host='')", "action", "A TCP readiness check."},
	{"workload_to_resource_function", "workload_to_resource_function(fn)", "None", "Customise how workloads map onto resource names."},
	{"enable_feature", "enable_feature(name)", "None", "Enable an experimental feature."},
	{"set_team", "set_team(team_id)", "None", "Associate this Tiltfile with a team."},
	{"struct", "struct(**kwargs)", "struct", "An immutable record with named fields."},
	{"local_git_repo", "local_git_repo(path)", "Repo", "Reference a local git repository."},
	{"docker_prune_settings", "docker_prune_settings(disable=False, max_age_mins=360, num_builds=0, interval_hrs=1, keep_recent=2)", "None", "Tune Tilt's automatic Docker image pruning."},
	{"ci_settings", "ci_settings(k8s_grace_period='', timeout='')", "None", "Configure `tilt ci` behaviour."},
	{"experimental_analytics_report", "experimental_analytics_report(payload)", "None", "Report a custom analytics event."},
}

var (
	_ starlsp.Host           = (*Host)(nil)
	_ starlsp.DialectOptions = (*Host)(nil)
)
