// Drives starlsp with Microsoft's own LSP client libraries.
//
// `vscode-jsonrpc` and `vscode-languageserver-protocol` are the layers VS Code's
// LanguageClient is built on: the same framing, the same request types, the same
// serialisation. A server that satisfies these satisfies the editor's client,
// and unlike a hand-written harness this cannot quietly agree with the server
// about what the protocol means.
//
// The editor chrome is the only thing missing, and that is covered separately by
// installing the extension into a real VS Code.

const assert = require("assert");
const cp = require("child_process");
const path = require("path");
const fs = require("fs");
const url = require("url");

// Everything comes from one package, so the request types the client sends are
// the exact objects the connection knows how to serialise. Mixing a separate
// vscode-jsonrpc copy in here produces "Unknown parameter structure byName".
const lsp = require("vscode-languageserver-protocol/node");

const FIXTURES = path.resolve(__dirname, "../../test-fixtures");
const SERVER = process.env.STARLSP_PATH || "starlsp";

function uriFor(name) {
  return url.pathToFileURL(path.join(FIXTURES, name)).toString();
}

function textOf(name) {
  return fs.readFileSync(path.join(FIXTURES, name), "utf8");
}

/** A live server, connected over stdio exactly as an editor connects. */
class Session {
  constructor(args = []) {
    this.stderr = "";
    this.child = cp.spawn(SERVER, args, { stdio: ["pipe", "pipe", "pipe"] });
    this.child.stderr.on("data", (d) => (this.stderr += d.toString()));
    this.exited = null;
    this.child.on("exit", (code, signal) => (this.exited = { code, signal }));

    this.conn = lsp.createProtocolConnection(
      new lsp.StreamMessageReader(this.child.stdout),
      new lsp.StreamMessageWriter(this.child.stdin)
    );
    this.diagnostics = new Map();
    this.conn.onNotification(lsp.PublishDiagnosticsNotification.type, (p) => {
      this.diagnostics.set(p.uri, p.diagnostics);
    });
    this.conn.listen();
  }

  async initialize() {
    const result = await this.conn.sendRequest(lsp.InitializeRequest.type, {
      processId: process.pid,
      clientInfo: { name: "starlsp-protocol-test", version: "1.0.0" },
      rootUri: url.pathToFileURL(FIXTURES).toString(),
      workspaceFolders: [
        { uri: url.pathToFileURL(FIXTURES).toString(), name: "fixtures" },
      ],
      capabilities: {
        textDocument: {
          synchronization: { dynamicRegistration: false, didSave: true },
          completion: { completionItem: { documentationFormat: ["markdown"] } },
          hover: { contentFormat: ["markdown"] },
          semanticTokens: {
            requests: { full: true },
            tokenTypes: [],
            tokenModifiers: [],
            formats: ["relative"],
          },
          rename: { prepareSupport: true },
        },
        workspace: { workspaceFolders: true },
      },
    });
    await this.conn.sendNotification(lsp.InitializedNotification.type, {});
    this.capabilities = result.capabilities;
    this.serverInfo = result.serverInfo;
    return result;
  }

  async open(name) {
    const uri = uriFor(name);
    await this.conn.sendNotification(lsp.DidOpenTextDocumentNotification.type, {
      textDocument: { uri, languageId: "starlark", version: 1, text: textOf(name) },
    });
    await this.settle();
    return uri;
  }

  /** Round-trip a request so every earlier notification has been processed. */
  async settle() {
    await this.conn.sendRequest(lsp.DocumentSymbolRequest.type, {
      textDocument: { uri: uriFor("clean.star") },
    });
  }

  async shutdown() {
    try {
      await this.conn.sendRequest(lsp.ShutdownRequest.type);
      await this.conn.sendNotification(lsp.ExitNotification.type);
    } catch {
      // the server may already be gone
    }
    this.conn.dispose();
    this.child.kill();
  }
}

// ---------- the suite ----------

const tests = [];
function test(name, fn) {
  tests.push({ name, fn });
}

test("initialize advertises every capability", async (s) => {
  const c = s.capabilities;
  assert.strictEqual(c.textDocumentSync, 2, "sync should be incremental");
  const expected = {
    completionProvider: c.completionProvider,
    hoverProvider: c.hoverProvider,
    signatureHelpProvider: c.signatureHelpProvider,
    definitionProvider: c.definitionProvider,
    referencesProvider: c.referencesProvider,
    documentHighlightProvider: c.documentHighlightProvider,
    renameProvider: c.renameProvider,
    documentSymbolProvider: c.documentSymbolProvider,
    workspaceSymbolProvider: c.workspaceSymbolProvider,
    foldingRangeProvider: c.foldingRangeProvider,
    selectionRangeProvider: c.selectionRangeProvider,
    documentLinkProvider: c.documentLinkProvider,
    semanticTokensProvider: c.semanticTokensProvider,
  };
  for (const [name, value] of Object.entries(expected)) {
    assert.ok(value, `capability ${name} not advertised`);
  }
  assert.ok(s.serverInfo && s.serverInfo.name.startsWith("starlsp"), "no serverInfo");
});

test("a clean file publishes no errors", async (s) => {
  const uri = await s.open("clean.star");
  const diags = s.diagnostics.get(uri) || [];
  const errors = diags.filter((d) => d.severity === lsp.DiagnosticSeverity.Error);
  assert.deepStrictEqual(errors.map((d) => d.message), [], "clean file reported errors");
});

test("a broken file publishes a parser error", async (s) => {
  const uri = await s.open("broken.star");
  const diags = s.diagnostics.get(uri) || [];
  assert.ok(diags.length > 0, "no diagnostics for a broken file");
  assert.ok(
    diags.some((d) => d.source === "starlark"),
    `no diagnostic from the Starlark parser: ${JSON.stringify(diags)}`
  );
});

test("completion offers the Starlark universe", async (s) => {
  const uri = await s.open("clean.star");
  const res = await s.conn.sendRequest(lsp.CompletionRequest.type, {
    textDocument: { uri },
    position: { line: 8, character: 7 },
  });
  const items = Array.isArray(res) ? res : res.items;
  const labels = items.map((i) => i.label);
  for (const want of ["len", "range", "print"]) {
    assert.ok(labels.includes(want), `completion missing ${want}`);
  }
});

test("completion after a dot offers string methods", async (s) => {
  const uri = await s.open("methods.star");
  const res = await s.conn.sendRequest(lsp.CompletionRequest.type, {
    textDocument: { uri },
    position: { line: 1, character: 16 },
  });
  const items = Array.isArray(res) ? res : res.items;
  const labels = items.map((i) => i.label);
  for (const want of ["upper", "startswith", "split"]) {
    assert.ok(labels.includes(want), `string completion missing ${want}: ${labels.slice(0, 8)}`);
  }
  const upper = items.find((i) => i.label === "upper");
  assert.ok(upper.detail, "completion item has no detail");
});

test("hover returns a signature", async (s) => {
  const uri = await s.open("clean.star");
  const hover = await s.conn.sendRequest(lsp.HoverRequest.type, {
    textDocument: { uri },
    position: { line: 2, character: 8 },
  });
  assert.ok(hover, "no hover returned for len");
  const value = hover.contents.value || hover.contents;
  assert.ok(String(value).includes("len"), `hover did not mention len: ${value}`);
});

test("document symbols outline the file", async (s) => {
  const uri = await s.open("clean.star");
  const symbols = await s.conn.sendRequest(lsp.DocumentSymbolRequest.type, {
    textDocument: { uri },
  });
  const names = symbols.map((x) => x.name);
  for (const want of ["greet", "main", "SIZE"]) {
    assert.ok(names.includes(want), `outline missing ${want}: ${names}`);
  }
});

test("definition points at the binding", async (s) => {
  const uri = await s.open("refs.star");
  const locs = await s.conn.sendRequest(lsp.DefinitionRequest.type, {
    textDocument: { uri },
    position: { line: 4, character: 13 },
  });
  assert.ok(locs && locs.length > 0, "no definition returned");
  assert.strictEqual(locs[0].range.start.line, 1, "definition is not the binding line");
});

test("references find every use", async (s) => {
  const uri = await s.open("refs.star");
  const locs = await s.conn.sendRequest(lsp.ReferencesRequest.type, {
    textDocument: { uri },
    position: { line: 4, character: 13 },
    context: { includeDeclaration: true },
  });
  assert.ok(locs.length >= 4, `expected >= 4 references, got ${locs.length}`);
});

test("document highlight marks the binding as a write", async (s) => {
  const uri = await s.open("refs.star");
  const hs = await s.conn.sendRequest(lsp.DocumentHighlightRequest.type, {
    textDocument: { uri },
    position: { line: 4, character: 13 },
  });
  assert.ok(hs.length >= 4, `expected >= 4 highlights, got ${hs.length}`);
  assert.ok(
    hs.some((h) => h.kind === lsp.DocumentHighlightKind.Write),
    "no write highlight for the binding"
  );
});

test("prepareRename accepts a local and refuses a builtin", async (s) => {
  const uri = await s.open("refs.star");
  const ok = await s.conn.sendRequest(lsp.PrepareRenameRequest.type, {
    textDocument: { uri },
    position: { line: 4, character: 13 },
  });
  assert.ok(ok, "prepareRename refused a local binding");

  const cleanUri = await s.open("clean.star");
  const refused = await s.conn.sendRequest(lsp.PrepareRenameRequest.type, {
    textDocument: { uri: cleanUri },
    position: { line: 2, character: 8 }, // `len`
  });
  assert.strictEqual(refused, null, "prepareRename allowed renaming a builtin");
});

test("rename rewrites every use", async (s) => {
  const uri = await s.open("refs.star");
  const edit = await s.conn.sendRequest(lsp.RenameRequest.type, {
    textDocument: { uri },
    position: { line: 4, character: 13 },
    newName: "sum_total",
  });
  const edits = edit.changes[uri];
  assert.ok(edits && edits.length >= 4, `rename produced ${edits ? edits.length : 0} edits`);
  for (const e of edits) assert.strictEqual(e.newText, "sum_total");
});

test("semantic tokens encode as quintuples", async (s) => {
  const uri = await s.open("clean.star");
  const tokens = await s.conn.sendRequest(lsp.SemanticTokensRequest.type, {
    textDocument: { uri },
  });
  assert.ok(tokens.data.length > 0, "no semantic tokens");
  assert.strictEqual(tokens.data.length % 5, 0, "token data is not a multiple of 5");
});

test("folding and selection ranges answer", async (s) => {
  const uri = await s.open("clean.star");
  const folds = await s.conn.sendRequest(lsp.FoldingRangeRequest.type, {
    textDocument: { uri },
  });
  assert.ok(folds.length > 0, "no folding ranges");

  const sel = await s.conn.sendRequest(lsp.SelectionRangeRequest.type, {
    textDocument: { uri },
    positions: [{ line: 2, character: 8 }],
  });
  assert.ok(sel.length > 0, "no selection ranges");
  let depth = 0;
  for (let r = sel[0]; r; r = r.parent) depth++;
  assert.ok(depth >= 2, `selection chain too shallow: ${depth}`);
});

test("workspace symbols search across files", async (s) => {
  await s.open("clean.star");
  const symbols = await s.conn.sendRequest(lsp.WorkspaceSymbolRequest.type, { query: "greet" });
  assert.ok(
    symbols.some((x) => x.name === "greet"),
    `workspace symbol search missed greet: ${symbols.map((x) => x.name)}`
  );
});

test("incremental edits keep positions correct", async (s) => {
  const uri = await s.open("clean.star");
  // Replace `greet` with `gree` on the call line, exactly as an editor would.
  await s.conn.sendNotification(lsp.DidChangeTextDocumentNotification.type, {
    textDocument: { uri, version: 2 },
    contentChanges: [
      {
        range: { start: { line: 8, character: 4 }, end: { line: 8, character: 9 } },
        text: "len",
      },
    ],
  });
  await s.settle();
  const hover = await s.conn.sendRequest(lsp.HoverRequest.type, {
    textDocument: { uri },
    position: { line: 8, character: 5 },
  });
  assert.ok(hover, "hover failed after an incremental edit");
  const value = String(hover.contents.value || hover.contents);
  assert.ok(value.includes("len"), `after the edit, hover says: ${value}`);
});

test("a document closed then reopened still works", async (s) => {
  const uri = await s.open("clean.star");
  await s.conn.sendNotification(lsp.DidCloseTextDocumentNotification.type, {
    textDocument: { uri },
  });
  await s.open("clean.star");
  const symbols = await s.conn.sendRequest(lsp.DocumentSymbolRequest.type, {
    textDocument: { uri },
  });
  assert.ok(symbols.length > 0, "reopened document produced no symbols");
});

test("an unknown method is answered, not fatal", async (s) => {
  try {
    await s.conn.sendRequest("textDocument/thisDoesNotExist", {
      textDocument: { uri: uriFor("clean.star") },
    });
    assert.fail("an unsupported method should return an error response");
  } catch (err) {
    assert.ok(err.code, "expected a JSON-RPC error, got: " + err);
  }
  // The connection must survive it.
  const symbols = await s.conn.sendRequest(lsp.DocumentSymbolRequest.type, {
    textDocument: { uri: uriFor("clean.star") },
  });
  assert.ok(Array.isArray(symbols), "connection did not survive an unknown method");
});

// ---------- runner ----------

async function main() {
  const session = new Session();
  let failures = 0;

  try {
    await session.initialize();
  } catch (err) {
    console.error("initialize failed:", err.message);
    console.error("server stderr:", session.stderr);
    process.exit(1);
  }

  for (const { name, fn } of tests) {
    try {
      await fn(session);
      console.log(`  ok    ${name}`);
    } catch (err) {
      failures++;
      console.log(`  FAIL  ${name}`);
      console.log(`        ${err.message}`);
    }
  }

  // The server must still be alive after all of that.
  if (session.exited) {
    console.log(`  FAIL  server exited during the run: ${JSON.stringify(session.exited)}`);
    console.log(`        stderr: ${session.stderr}`);
    failures++;
  } else {
    console.log("  ok    server survived the whole session");
  }

  await session.shutdown();

  console.log(`\n${tests.length - failures}/${tests.length} passed`);
  process.exit(failures > 0 ? 1 : 0);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
