"use strict";
Object.defineProperty(exports, "__esModule", { value: true });
exports.activate = activate;
exports.deactivate = deactivate;
const vscode_1 = require("vscode");
const node_1 = require("vscode-languageclient/node");
const resourceView_1 = require("./resourceView");
let client;
let resourceViewProvider;
function createClient() {
    const config = vscode_1.workspace.getConfiguration("infracost");
    const serverPath = config.get("serverPath", "infracost-lsp");
    const env = { ...process.env };
    const debugUI = config.get("debugUI", "");
    if (debugUI) {
        env.INFRACOST_DEBUG_UI = debugUI;
    }
    const serverOptions = {
        command: serverPath,
        transport: node_1.TransportKind.stdio,
        options: { env },
    };
    const clientOptions = {
        documentSelector: [
            { scheme: "file", language: "terraform" },
            { scheme: "file", language: "yaml" },
            { scheme: "file", language: "json" },
        ],
        synchronize: {
            configurationSection: "infracost",
        },
    };
    return new node_1.LanguageClient("infracost", "Infracost", serverOptions, clientOptions);
}
function activate(context) {
    client = createClient();
    client.start().then(() => checkAuthStatus());
    resourceViewProvider = new resourceView_1.ResourceViewProvider();
    context.subscriptions.push(vscode_1.window.registerWebviewViewProvider(resourceView_1.ResourceViewProvider.viewType, resourceViewProvider));
    // Move the sidebar view to the secondary sidebar on first install.
    const movedKey = "infracost.resourceDetailsMoved";
    if (!context.globalState.get(movedKey)) {
        vscode_1.commands
            .executeCommand("vscode.moveViews", {
            viewIds: [resourceView_1.ResourceViewProvider.viewType],
            destinationId: "workbench.view.extension.infracost-secondary",
        })
            .then(() => context.globalState.update(movedKey, true), () => {
            vscode_1.commands
                .executeCommand("workbench.action.moveView", {
                id: resourceView_1.ResourceViewProvider.viewType,
                to: "auxiliarybar",
            })
                .then(() => context.globalState.update(movedKey, true), () => { });
        });
    }
    let debounceTimer;
    context.subscriptions.push(vscode_1.window.onDidChangeTextEditorSelection((e) => {
        if (!client || !isSupportedFile(e.textEditor.document.uri.fsPath)) {
            return;
        }
        if (debounceTimer) {
            clearTimeout(debounceTimer);
        }
        debounceTimer = setTimeout(async () => {
            const uri = e.textEditor.document.uri.toString();
            const line = e.selections[0].active.line;
            try {
                const result = await client.sendRequest("infracost/resourceDetails", { uri, line });
                resourceViewProvider.update(result);
            }
            catch {
                // Ignore errors (e.g. server not ready)
            }
        }, 150);
    }));
    // Triggered by code lens clicks — fetches resource details and reveals the sidebar.
    context.subscriptions.push(vscode_1.commands.registerCommand("infracost.revealResource", async (uri, line) => {
        if (!client) {
            return;
        }
        try {
            const result = await client.sendRequest("infracost/resourceDetails", { uri, line });
            resourceViewProvider.update(result);
            vscode_1.commands.executeCommand(`${resourceView_1.ResourceViewProvider.viewType}.focus`);
        }
        catch {
            // Ignore errors
        }
    }));
    context.subscriptions.push(vscode_1.commands.registerCommand("infracost.login", async () => {
        if (!client) {
            return;
        }
        try {
            const result = await client.sendRequest("infracost/login");
            const choice = await vscode_1.window.showInformationMessage(`Enter code ${result.userCode} at ${result.verificationUri}`, "Open Browser", "Copy Code");
            if (choice === "Open Browser") {
                await vscode_1.env.openExternal(vscode_1.Uri.parse(result.verificationUriComplete));
            }
            else if (choice === "Copy Code") {
                await vscode_1.env.clipboard.writeText(result.userCode);
                await vscode_1.env.openExternal(vscode_1.Uri.parse(result.verificationUriComplete));
            }
            // Clear the login view — the server will show "Scanning..." once auth completes.
            resourceViewProvider.update({ scanning: false });
        }
        catch (e) {
            vscode_1.window.showErrorMessage(`Infracost login failed: ${e}`);
        }
    }));
    context.subscriptions.push(vscode_1.commands.registerCommand("infracost.restartLsp", async () => {
        if (client) {
            await client.stop();
        }
        client = createClient();
        await client.start();
    }));
}
const cfnPatterns = ["template", "cloudformation", "cfn", "stack", "infracost"];
function isSupportedFile(fsPath) {
    if (fsPath.endsWith(".tf")) {
        return true;
    }
    const base = fsPath.split(/[\\/]/).pop()?.toLowerCase() ?? "";
    if (base.endsWith(".yml") || base.endsWith(".yaml") || base.endsWith(".json")) {
        return cfnPatterns.some((p) => base.includes(p));
    }
    return false;
}
async function checkAuthStatus() {
    if (!client) {
        return;
    }
    try {
        const result = await client.sendRequest("infracost/resourceDetails", { uri: "", line: 0 });
        if (result.needsLogin) {
            resourceViewProvider.showLogin();
        }
    }
    catch {
        // Ignore — server may not be ready yet
    }
}
function deactivate() {
    if (!client) {
        return undefined;
    }
    return client.stop();
}
//# sourceMappingURL=extension.js.map