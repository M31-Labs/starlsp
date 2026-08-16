// Bundle the extension into one file.
//
// A VS Code extension must ship the modules it requires. Bundling is preferred
// over shipping node_modules: it is smaller, it loads faster, and it makes the
// missing-dependency failure impossible rather than merely unlikely.
const esbuild = require("esbuild");

const production = process.argv.includes("--production");

esbuild
  .build({
    entryPoints: ["src/extension.js"],
    bundle: true,
    outfile: "dist/extension.js",
    platform: "node",
    format: "cjs",
    target: "node18",
    // `vscode` is provided by the editor at runtime and must never be bundled.
    external: ["vscode"],
    minify: production,
    sourcemap: !production,
    logLevel: "info",
  })
  .catch(() => process.exit(1));
