"""A rule that uses the Bazel builtins the dialect host must know."""

def _impl(ctx):
    out = ctx.actions.declare_file(ctx.attr.name + ".txt")
    ctx.actions.write(output = out, content = "hello")
    return [DefaultInfo(files = depset([out]))]

greeting = rule(
    implementation = _impl,
    attrs = {
        "message": attr.string(default = "hello"),
        "deps": attr.label_list(allow_files = True),
    },
)
