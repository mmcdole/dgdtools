# Configuration & Mudlib Profiles

`dgdtools` is configured with a single YAML file named `dgdtools.yml` (or `.dgdtools.yml`). Every CLI command and the language server read the same config file, so editor diagnostics and formatting always match the command line.

Every key is optional, and `dgdtools` works without any config file (running file-local lint rules and default formatting).

## Config discovery

Given a file or directory, `dgdtools` searches upward for the nearest `dgdtools.yml` (or `.dgdtools.yml`), so commands work from anywhere inside a mudlib.

A typical full-mudlib layout puts `dgdtools.yml` beside the driver config and mudlib directory:

```text
mud/
├── dgdtools.yml
├── etc/mud.dgd     # DGD driver config
└── lib/            # root: lib
```

See [dgdtools.example.yml](../dgdtools.example.yml) for a short commented reference and [examples/viking/dgdtools.yml](../examples/viking/dgdtools.yml) for a real-world mudlib configuration.

### Example `dgdtools.yml`

```yaml
root: lib                     # Directory that corresponds to mudlib "/"
driver_config: etc/mud.dgd    # DGD driver config: include_dirs, include_file,
                              # auto_object, driver_object, typechecking
exclude:                      # Glob patterns (** crosses directories)
  - "**/old/**"

conventions:
  macros: { public: specifier }   # #define public, for files read without it
  # Mudlib functions that invoke a callback by name (0-based argument indices).
  # call_other and call_out are built in.
  call_registry:
    add_action: { function: 0 }
    bind: [{ object: 0, function: 1 }, { function: 0 }]

profile_build:
  name: mymud
  share: ["kernel/**", "lib/**"]
  checkouts: ["usr/*"]

lint:
  rules:                      # Rule name, code, or group -> off | info | warning | error
    unreachable-code: warning
    raw-inherit-path: { severity: warning, deny: ["/lib/"] }
  overrides:
    - paths: ["usr/**"]
      rules: { runtime: off }
```

Unknown keys and invalid values are errors rather than silently ignored; a misspelled key gets a "did you mean" suggestion, and a key that older versions of dgdtools had says what replaced it (see [Removed keys](#removed-keys)).

Configuration keys fall into two categories:
- **Project keys** (top-level) describe the mudlib itself. In a partial checkout, the driver settings the config leaves unset come from the mudlib profile, and the dialect and conventions it leaves unset do too.
- **Settings** (`format:` and `lint:`) control formatting and linting behavior and can be customized per directory.

---

## Project keys

| Key | Description |
|---|---|
| `root` | Directory representing mudlib `/`, relative to the config file (`root: .` for the config's own directory). Never guessed: without `root`, `profile`, or `lib_path`, only file-local rules run. If `profile` or `lib_path` is set without `root`, the config file's directory is treated as a partial checkout. |
| `profile` | Mudlib profile to work against: a profile name (looked up as `NAME.profile`) or a file path. Supplies driver settings, shared headers, and shared object signatures. Local files on disk always override the profile. |
| `lib_path` | The mudlib path of a partial checkout (e.g., `/usr/ann`) when its filesystem path doesn't match the profile's `checkouts` patterns. |
| `exclude` | Globs relative to the mudlib root that are skipped during directory walks. Files named explicitly on the command line are always processed. |
| `driver_config` | The DGD driver's config file, relative to `dgdtools.yml`: where the driver settings are read from, for whole-mudlib lint, the language server, and `dgdtools profile build`. dgdtools reads `include_dirs` (include dirs without a leading `/` are relative to the including file; dirs starting with `~` are treated as unknown), `include_file` (force-included header), `auto_object`, `driver_object` (whose driver-called functions cannot be renamed), and `typechecking`. A project with a profile gets these from the profile instead, and does not read `driver_config`. |
| `driver` | The driver settings written in `dgdtools.yml`, for a mudlib without its driver config at hand: `include_dirs`, `include_file`, `auto_object`, `driver_object`, and `typechecking`, named and read as in the driver config (e.g., `driver: { include_dirs: [/include], auto_object: /kernel/auto, typechecking: 2 }`). Each key is optional. One that is set wins over `driver_config` and the profile; the rest come from them. |
| `dialect` | Driver build flags and extensions: `slash_slash` (default `true`), `closures` (default `false`), and `kfuns` (extra extension-module kfun prototypes, e.g., `"int *rusage()"` or `"string sha256(string text, varargs int rounds)"`). Stock DGD kfuns are built in. |
| `relative_object_paths` | Base for object paths without a leading slash (like `"std/room"` in `inherit` or `call_other`): `root` (default, matching `compile_object`) or `file` (relative to the current file's directory, as in the kernel library). |
| `conventions` | Mudlib-specific patterns (none enabled by default):<br>• `create`: the function the mudlib's objects define to be initialized (default `create`). Set it when objects define another name; the driver config's `create` is not used, since in a mudlib whose auto object receives the driver's call and passes it on (like the kernel library's `_F_create` → `create`), it is not what objects define.<br>• `macros`: macro classifications (`expression`, `empty`, `specifier`, `type`, `string`, `statement`, `opaque`) overriding what is inferred from a `#define` and covering files read without the header that defines the macro, e.g. `{ public: specifier }` for a mudlib's `#define public`.<br>• `defines`: macro names treated as defined for `#ifdef` evaluation.<br>• `call_registry`: functions that invoke a function by name, specifying 0-based `function`, optional `object` (defaults to `this_object()`), and optional `args` indices; multiple overloads can be listed (`call_other` and `call_out` are built in).<br>• `object_registry`: functions taking an object path string argument (`compile_object`, `find_object`, `clone_object` are built in).<br>• `autosave_markers`: symbols indicating an object persists state via `save_object`.<br>• `virtual_paths`: mudlib path globs served by virtual-object daemons (no backing `.c` file). |
| `profile_build` | Used by maintainers with the full mudlib: `name`, `share` (globs of shared code), `share_min_owners` (default `5`; `0` shares only what `share` lists), `omit` (files to exclude from profiles), and `checkouts` (per-wizard/domain directory patterns, e.g., `["usr/*"]`, also used to place partial checkouts and group rename diffs). |

---

## Format & lint settings

| Key | Description |
|---|---|
| `format.indent_style` | `space` (default) or `tab`. |
| `format.indent_width` | Spaces per indent level, `1`–`16` (default `4`). |
| `format.line_endings` | `preserve` (default), `lf`, or `crlf`. |
| `lint.rules` | Maps a rule name, code (`E001`), or group (`all`, `syntax`/`E`, `runtime`/`R`, `suspicious`/`S`, `convention`/`C`) to `off`, `info`, `warning`, or `error`, or to a map with `severity` and rule-specific options (`dgdtools explain RULE` lists options). Setting a severity enables the rule. Unknown options or configuring the same rule by both name and code are errors. |
| `lint.overrides` | List of `{ paths: [globs], rules: { ... } }` blocks for path-specific rule overrides. |
| `lint.baseline` | Baseline file path (default `dgdtools-baseline.json` next to the config). Root config only. |

The severity that makes `dgdtools lint` exit `1` and the per-file cap on findings are properties of a run, not of the project: pass `--fail-on` (default `error`) and `--max-per-file`.

---

## Per-directory settings

A nested `dgdtools.yml` containing only `format:` and `lint:` (or explicitly setting `root: false`) acts as a **settings scope** for its subtree while still belonging to the parent project:

```yaml
# usr/ann/dgdtools.yml
root: false
format: { indent_style: tab }
lint:
  rules: { missing-visibility: warning }
  overrides:
    - paths: ["attic/**"]          # Relative to this directory
      rules: { all: off }
```

Putting a project key in a file with `root: false` is an error. Conversely, a nested `dgdtools.yml` that *does* contain a project key (such as a wizard folder with `profile: NAME` inside a full mudlib) defines a separate project; linting the parent project skips it with a notice.

### Precedence

For scalar settings, the highest-priority source wins:

1. Command-line flags
2. Nearest nested settings scope, then outer scopes
3. Project `dgdtools.yml` (including its `driver:` block)
4. The mudlib profile, or without one `driver_config` (driver settings; from the profile also dialect and conventions)
5. `.editorconfig` (`indent_style`, `indent_size`, `end_of_line` for the three `format` keys)
6. Built-in defaults

For `lint.rules`, settings accumulate from the outside in (project rules → project overrides → outer scopes → inner scopes), so the closest config has the final word.

### User fallback and CLI overrides

- **User config:** If no project `dgdtools.yml` is found, `dgdtools` falls back to `~/.config/dgdtools/dgdtools.yml`—useful for setting a default `profile:` when working on bare wizard checkouts. User config is never merged with a project config.
- **`--config FILE`:** Uses only `FILE` for all paths and disables upward config discovery.
- **`--no-config`:** Ignores all configuration files.

### Validating configuration

Run `dgdtools config` to validate a project's `dgdtools.yml` and all nested settings scopes (exits `2` with file and line details on any error). Pass a file path—`dgdtools config path/to/file.c`—to inspect the resolved settings for that file and where each value came from.

### Removed keys

dgdtools 0.4 trimmed the configuration. A config that still uses an old key fails with a message naming the file, the line, and what replaces it:

| Old key | Now |
|---|---|
| `profile_build.driver_config` | Top-level `driver_config` |
| `include_dirs`, `include_file`, `auto_objects`, `driver_object`, `dialect.typechecking` | The `driver:` block (`auto_objects` is `driver.auto_object`), or read from `driver_config` (a partial checkout gets them from its profile) |
| `create` | `conventions.create` (default `create`) |
| `conventions.specifier_macros: [public]` | `conventions.macros: { public: specifier }` |
| `conventions.format_registry` | Removed; `sscanf` is built in |
| `extends` | Removed; use nested `root: false` settings scopes, and a profile to share a mudlib's setup |
| A rule's `max_per_file` | `--max-per-file` on the command line |
| `lint.fail_on` | `--fail-on` on the command line |
| `profile_build.share_min_owners: -1` | `share_min_owners: 0` |

Profiles changed with it (format 2): rebuild them with `dgdtools profile build`. An older profile is refused with a message saying so.

---

## Mudlib profiles

A **mudlib profile** (`<name>.profile`) is a compact interface file published by a mudlib's maintainers so wizards can lint, navigate, and complete code in a partial checkout without downloading the whole mudlib.

A profile contains:
- The mudlib's driver settings (what `profile build` took from `driver:` and `driver_config`), dialect flags, and conventions
- Shared header files (so `#include` directives and macros resolve)
- Public stubs of shared objects: `#define`s and `#include`s of shared headers, `inherit` statements, and all non-private function and global variable declarations with bodies replaced by `{}`, each with the comment above it that hover shows

It never includes function bodies, other comments, private symbols, or anything matched by `profile_build.omit`. For reference, VikingMUD's 720 shared files produce a 149 KB profile.

### For wizards (using a profile)

1. Download your mud's `<name>.profile` file and place it in one of the profile search locations (checked in order):
   1. Next to the `dgdtools.yml` that references it
   2. Any directory in `$DGDTOOLS_PROFILE_PATH`
   3. Your user config directory: `~/.config/dgdtools/profiles/` (on macOS also `~/Library/Application Support/dgdtools/profiles/`, on Windows `%AppData%\dgdtools\profiles\`)

   Alternatively, point `profile:` to an explicit file path. Run `dgdtools profile show` to list the search directories on your system.

2. Add a `dgdtools.yml` in your checkout directory:

   ```yaml
   profile: mymud
   # Optional: only needed if your local folder path doesn't match the
   # mudlib's checkout patterns (e.g., usr/ann):
   # lib_path: /usr/ann
   ```

Anything neither in your local checkout nor in the profile (such as another wizard's directory) is treated as *unknown* rather than flagged as missing. `dgdtools` never assumes a profile automatically—no profiles are bundled with `dgdtools`.

### For mudlib maintainers (building a profile)

From a full mudlib checkout whose `dgdtools.yml` has `root:`, `driver_config:` (or a `driver:` block), and a `profile_build:` section:

```sh
dgdtools profile build                     # Generates dgdtools.profile
dgdtools profile show mymud                # Inspect summary of a profile
dgdtools profile show mymud /std/room.c    # Print the generated stub for one file
```

Shared files are determined automatically by cross-owner usage (any file inherited, included, or called by at least `share_min_owners` distinct owners; `0` turns this off) in addition to the explicit `share:` globs. Rename the generated `dgdtools.profile` to `<mud>.profile` and distribute it to your wizards.
