// Proves the packaged extension actually loads.
//
// This exists because of a real failure. The extension was first packaged with
// `vsce package --no-dependencies`, which ships the extension's own code but
// none of the modules it requires. It installed cleanly, VS Code activated it,
// and the activation threw:
//
//     Error: Cannot find module 'vscode-languageclient/node'
//         at .../m31labs.starlsp-0.2.0/src/extension.js:12:5
//
// No protocol test could have caught that, because no protocol test loads the
// extension. The fix is to bundle with esbuild; this test is what keeps it
// fixed.
//
// The bundle is loaded with `vscode` stubbed, exactly as the editor provides
// it, and with node_modules made unreachable, exactly as a user's machine has
// it. If a dependency ever escapes the bundle again, this fails.

const assert = require("assert");
const path = require("path");
const fs = require("fs");
const Module = require("module");

const ROOT = path.resolve(__dirname, "../..");
const BUNDLE = path.join(ROOT, "dist", "extension.js");

/** A stand-in for the editor's `vscode` module.
 *
 * vscode-languageclient subclasses editor types at module load — for example
 * `class extends vscode.CompletionItem` — so a stub with a fixed set of keys
 * breaks on whichever type the library happens to extend next. Unknown keys
 * therefore resolve to a generic class, which is valid both as a constructor
 * and as a base class.
 */
function vscodeStub() {
  const disposable = { dispose() {} };
  const known = {
    workspace: {
      getConfiguration: () => ({ get: () => undefined }),
      createFileSystemWatcher: () => disposable,
      onDidChangeConfiguration: () => disposable,
      onDidOpenTextDocument: () => disposable,
      onDidChangeTextDocument: () => disposable,
      onDidCloseTextDocument: () => disposable,
      onDidSaveTextDocument: () => disposable,
      textDocuments: [],
    },
    window: {
      showErrorMessage: () => {},
      showInformationMessage: () => {},
      showWarningMessage: () => {},
      createOutputChannel: () => ({ appendLine() {}, show() {}, dispose() {} }),
      onDidChangeActiveTextEditor: () => disposable,
    },
    commands: { registerCommand: () => disposable, executeCommand: async () => undefined },
    languages: { match: () => 0, registerCodeActionsProvider: () => disposable },
    env: { language: "en", openExternal: async () => true },
    version: "1.85.0",
    Uri: { file: (p) => ({ fsPath: p, toString: () => "file://" + p }), parse: (s) => ({ toString: () => s }) },
    EventEmitter: class {
      constructor() { this.event = () => disposable; }
      fire() {}
      dispose() {}
    },
    Disposable: class {
      constructor(fn) { this._fn = fn; }
      dispose() {}
      static from() { return disposable; }
    },
  };

  return new Proxy(known, {
    get(target, prop) {
      if (prop in target) return target[prop];
      if (typeof prop === "symbol") return undefined;
      // Anything else the library reaches for is treated as a constructible
      // class, which is what `extends` requires.
      return class Generic {
        constructor(...args) {
          this.args = args;
        }
      };
    },
    has() {
      return true;
    },
  });
}

function loadBundle(bundlePath) {
  // Intercept `require("vscode")`, which only the editor can provide.
  const originalLoad = Module._load;
  Module._load = function (request, parent, isMain) {
    if (request === "vscode") return vscodeStub();
    return originalLoad.call(this, request, parent, isMain);
  };
  try {
    delete require.cache[require.resolve(bundlePath)];
    return require(bundlePath);
  } finally {
    Module._load = originalLoad;
  }
}

const checks = [];
function check(name, fn) {
  checks.push({ name, fn });
}

check("the bundle exists and was built", () => {
  assert.ok(fs.existsSync(BUNDLE), `${BUNDLE} is missing; run \`npm run build\``);
  const size = fs.statSync(BUNDLE).size;
  assert.ok(size > 50_000, `bundle is only ${size} bytes; the client is probably not bundled`);
});

check("package.json main points at the bundle", () => {
  const pkg = JSON.parse(fs.readFileSync(path.join(ROOT, "package.json"), "utf8"));
  assert.strictEqual(
    pkg.main,
    "./dist/extension.js",
    "main must point at the bundle, not at src/, or the published extension will not load"
  );
});

check("the bundle loads and exports activate/deactivate", () => {
  const ext = loadBundle(BUNDLE);
  assert.strictEqual(typeof ext.activate, "function", "no activate export");
  assert.strictEqual(typeof ext.deactivate, "function", "no deactivate export");
});

check("the language client is bundled in, not required from node_modules", () => {
  const source = fs.readFileSync(BUNDLE, "utf8");
  assert.ok(
    !/require\(["']vscode-languageclient/.test(source),
    "the bundle still requires vscode-languageclient at runtime; it is not bundled"
  );
  assert.ok(
    /require\(["']vscode["']\)/.test(source),
    "the bundle should still require `vscode`, which the editor provides"
  );
});

check("the packaged vsix carries the bundle and no src/", () => {
  const vsix = fs
    .readdirSync(ROOT)
    .filter((f) => f.endsWith(".vsix"))
    .sort()
    .pop();
  if (!vsix) {
    console.log("       (no .vsix built; skipping)");
    return;
  }
  const { execFileSync } = require("child_process");
  const listing = execFileSync("unzip", ["-l", path.join(ROOT, vsix)], { encoding: "utf8" });
  assert.ok(listing.includes("extension/dist/extension.js"), "vsix has no bundle");
  assert.ok(!/extension\/src\//.test(listing), "vsix still ships src/");
  assert.ok(!/node_modules/.test(listing), "vsix ships node_modules; it should be bundled instead");
});

check("the installed copy, if present, is the bundled one", () => {
  const installed = path.join(
    process.env.HOME,
    ".vscode-server/extensions/m31labs.starlsp-0.2.0"
  );
  if (!fs.existsSync(installed)) {
    console.log("       (extension not installed in a WSL remote; skipping)");
    return;
  }
  const pkg = JSON.parse(fs.readFileSync(path.join(installed, "package.json"), "utf8"));
  assert.strictEqual(pkg.main, "./dist/extension.js");
  assert.ok(
    fs.existsSync(path.join(installed, "dist", "extension.js")),
    "installed extension has no bundle"
  );
  // And it must load, with node_modules nowhere near it.
  const ext = loadBundle(path.join(installed, "dist", "extension.js"));
  assert.strictEqual(typeof ext.activate, "function", "the installed bundle does not load");
});

let failures = 0;
for (const { name, fn } of checks) {
  try {
    fn();
    console.log(`  ok    ${name}`);
  } catch (err) {
    failures++;
    console.log(`  FAIL  ${name}`);
    console.log(`        ${err.message}`);
  }
}
console.log(`\n${checks.length - failures}/${checks.length} passed`);
process.exit(failures > 0 ? 1 : 0);
