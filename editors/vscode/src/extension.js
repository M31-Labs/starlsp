// Starlark language client for VS Code.
//
// The client is deliberately thin. It starts `starlsp` and hands the buffer
// over; everything an editor shows comes from the server, so a capability added
// there needs no change here.

const { workspace, window, commands } = require("vscode");
const {
  LanguageClient,
  TransportKind,
  RevealOutputChannelOn,
} = require("vscode-languageclient/node");

/** @type {LanguageClient | undefined} */
let client;
/** @type {Error | undefined} */
let lastStartError;

/** Resolve the server binary.
 *
 * STARLSP_PATH wins so a test run, a CI job, or a developer working on the
 * server can point the extension at a specific build without editing settings.
 */
function serverCommand() {
  return (
    process.env.STARLSP_PATH ||
    workspace.getConfiguration("starlsp").get("serverPath") ||
    "starlsp"
  );
}

function serverOptions() {
  const config = workspace.getConfiguration("starlsp");
  const command = serverCommand();
  const dialect = config.get("dialect") || "auto";

  const args = [];
  if (dialect && dialect !== "auto") {
    args.push("--dialect", dialect);
  }

  const run = { command, args, transport: TransportKind.stdio };
  return { run, debug: run };
}

function clientOptions() {
  return {
    documentSelector: [{ scheme: "file", language: "starlark" }],
    synchronize: {
      // A dialect host resolves load() through these, so a change to either can
      // change what go-to-definition finds.
      fileEvents: workspace.createFileSystemWatcher(
        "**/{WORKSPACE,WORKSPACE.bazel,MODULE.bazel,Tiltfile,mod.yaml,mod.lock}"
      ),
    },
    outputChannelName: "Starlark",
    // A failed request should not steal focus from the editor.
    revealOutputChannelOn: RevealOutputChannelOn.Never,
  };
}

async function start(context) {
  client = new LanguageClient(
    "starlsp",
    "Starlark Language Server",
    serverOptions(),
    clientOptions()
  );

  try {
    await client.start();
    lastStartError = undefined;
  } catch (err) {
    lastStartError = err instanceof Error ? err : new Error(String(err));
    const command = serverCommand();
    window.showErrorMessage(
      `Starlark: could not start "${command}". Install it with ` +
        "`go install github.com/M31-Labs/starlsp/cmd/starlsp@latest`, " +
        `then set starlsp.serverPath if it is not on your PATH. (${lastStartError.message})`
    );
    client = undefined;
    return;
  }
  context.subscriptions.push({ dispose: () => client && client.stop() });
}

async function activate(context) {
  context.subscriptions.push(
    commands.registerCommand("starlsp.restart", async () => {
      if (client) {
        await client.stop();
        client = undefined;
      }
      await start(context);
      window.showInformationMessage("Starlark: language server restarted.");
    })
  );

  context.subscriptions.push(
    commands.registerCommand("starlsp.showSurface", async () => {
      const channel = window.createOutputChannel("Starlark Server Surface");
      channel.appendLine(
        "Run `starlsp --probe` in a terminal to see the names this server offers."
      );
      channel.show();
    })
  );

  await start(context);

  // Exported for the integration tests, so they can wait for readiness rather
  // than sleeping and hoping.
  return {
    isReady: () => Boolean(client),
    client: () => client,
    // Surfaced so a failing test says why, rather than only that it failed.
    lastError: () => lastStartError,
    serverCommand,
  };
}

async function deactivate() {
  if (client) {
    await client.stop();
    client = undefined;
  }
}

module.exports = { activate, deactivate };
