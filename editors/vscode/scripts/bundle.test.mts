// The bundle the package ships loads with only `vscode` outside it, and
// registers the commands package.json contributes. (jsonc-parser's UMD
// build once left a require of a file the bundle did not have, and the
// published extension failed to activate.)
import assert from 'node:assert/strict';
import fs from 'node:fs';
import Module from 'node:module';
import path from 'node:path';
import test from 'node:test';

const root = path.join(import.meta.dirname, '..');

// The names of the vscode API's namespaces, enums and classes.
const vscodeNames = Object.fromEntries(
  (fs.readFileSync(path.join(root, 'node_modules', '@types', 'vscode', 'index.d.ts'), 'utf8')
    .match(/^\t(?:export )?(?:namespace|enum|class) (\w+)/gm) ?? [])
    .map((m) => [m.trim().split(' ').pop()!, true]),
);

test('the bundle loads and activates', () => {
  const bundle = path.join(root, 'dist', 'extension.js');
  assert.ok(fs.existsSync(bundle), 'run npm run build first');
  const registered: string[] = [];
  const anything: any = new Proxy(function () {}, {
    get: (_t, k) => (k === 'then' ? undefined : anything),
    apply: () => anything,
    construct: () => anything,
  });
  // A plain object, so that the bundle's import of it sees its names.
  const vscode: Record<string, unknown> = {};
  for (const name of Object.getOwnPropertyNames(vscodeNames)) vscode[name] = anything;
  vscode.commands = { registerCommand: (id: string) => (registered.push(id), anything), executeCommand: anything };
  const require = Module.createRequire(bundle);
  const load = (Module as any)._load;
  (Module as any)._load = function (request: string, ...rest: unknown[]) {
    return request === 'vscode' ? vscode : load.call(this, request, ...rest);
  };
  try {
    const ext = require(bundle);
    ext.activate({ subscriptions: [], extensionPath: root, workspaceState: anything, globalState: anything });
  } finally {
    (Module as any)._load = load;
  }
  const manifest = JSON.parse(fs.readFileSync(path.join(root, 'package.json'), 'utf8'));
  for (const c of manifest.contributes.commands) {
    assert.ok(registered.includes(c.command), `${c.command} is not registered`);
  }
});
