# Configuration and command line: design

One binary, `dgdtools`; one configuration file, `dgdtools.yml`. This
document says how the file is laid out, how the settings of one source
file are found, and what the command looks like. The linter and the
project model it needs are in [architecture.md](architecture.md); the
formatter is in [formatter.md](formatter.md).

## 1. What the others do

| Tool | File | Found | Nested files | Editor settings |
|---|---|---|---|---|
| ruff | `ruff.toml` / `pyproject.toml`, sections `lint` and `format` | nearest, walking up from each file | the nearest one is the whole configuration of the files under it (no merge unless it says `extend`) | reads none; `~/.config/ruff/ruff.toml` only when no project file is found |
| Biome | `biome.json`, sections `formatter`, `linter` | walking up from the working directory | a nested file must say `"root": false`; it then adjusts settings for its subtree and inherits the rest; a second root is an error | `.editorconfig` opt-in |
| Prettier | `.prettierrc` | nearest per file, no merge; `overrides` by glob inside one file | nearest wins whole | `.editorconfig` read for indent, width, end of line unless the Prettier file says otherwise |
| clang-format / clang-tidy | `.clang-format`, `.clang-tidy` | nearest per file | nearest wins whole, unless it says `BasedOnStyle: InheritParentConfig` (`InheritParentConfig: true`), which merges it over the next one up | none |
| rustfmt | `rustfmt.toml` | nearest per file | nearest wins whole | none |
| EditorConfig | `.editorconfig` | every file up to one with `root = true` | all apply; nearer and later sections win per property | it is the editor setting |

What to take from them:

- **One file for all tools of a language** (ruff, Biome, Deno): the
  linter and the formatter share what a project *is* (here: where the
  lib root is, the dialect, the profile), so two files would state it
  twice.
- **Per-file discovery** is what makes a tool work from any directory
  and in an editor.
- **Nested files are wanted, and whole-file replacement is wrong for a
  tool with a project model.** A wizard's directory legitimately wants
  different lint severities or its own indentation. It cannot
  legitimately have a different lib root: there is one `/`. Biome's
  split, a root file that owns the project and nested files that only
  adjust settings, is the one that fits; clang's `InheritParentConfig`
  is the same idea spelled as an opt-in.
- **`.editorconfig` is the lowest common denominator for indentation and
  line endings**, already in repositories and already obeyed by editors.
  Reading it means a project that has one needs no `format:` section.
- **A user-level file is a fallback, never an override** (ruff). A
  project's configuration is the same for everyone who works on it.

## 2. The file

`dgdtools.yml` (also `.dgdtools.yml`), YAML. Unknown keys and values are
errors.

```yaml
# Project keys: what the project is. Root config only.
root: lib                  # the directory that is "/" to the driver
profile: mylib             # the mudlib profile, for a partial checkout
lib_path: /usr/name        # where a partial checkout sits in the lib
exclude: ["attic/**"]
driver_config: etc/mud.dgd # the DGD driver's config file
dialect: {slash_slash: true, closures: false}
# dialect.kfuns: ["int *rusage()"]   the kfuns of the driver's extension modules
relative_object_paths: file
conventions:               # the lib's habits: macros, registries; no defaults
  create: create           # what the lib's objects define (default create)
  macros: {public: specifier}
  call_registry: {subscribe: {function: 1}}
profile_build: {name: mylib, share: ["kernel/**"], checkouts: ["usr/*"]}

# Settings: how files are formatted and linted. Any scope.
format:
  indent_style: space
  indent_width: 4
  line_endings: preserve
lint:
  rules: {raw-inherit-path: warning}
  overrides:
    - paths: ["usr/**"]
      rules: {lifecycle-chain: off}
  baseline: dgdtools-baseline.json   # root config only
```

The top level holds the project keys; `lint:` and `format:` hold
settings and nothing else. The split is the point: everything under the
two sections can differ per directory, nothing above them can.

**The driver's settings have one source**, the driver's own config file:
`driver_config` names it, and `include_dirs`, `include_file`,
`auto_object`, `driver_object` and `typechecking` are read from it, for
a whole-lib lint and for a profile build alike. They are not keys of
`dgdtools.yml`: a second place to state them is a second place for them
to be wrong. A partial checkout has no driver config; it gets them from
its profile, which carries what the build read. (`dialect.typechecking`
is the one that can still be said in the file, for a lib without a
driver config at hand; the file's word wins.) The driver config's
`create` is not read: it is what the auto object receives, and in a lib
whose auto object passes it on (the kernel library's `_F_create`), what
objects define is `conventions.create`, default `create`.

There is no `extends:`. Per-directory settings are nested files (section
3), and what a mudlib shares with its wizards is its profile; the two
cover what including another config file was for, without a merge rule
to learn.

Keys of dgdtools before 0.4 are errors that say what replaced them
(`profile_build.driver_config moved: use top-level driver_config`,
`include_dirs removed: dgdtools reads it from driver_config ...`,
`conventions.specifier_macros removed: use conventions.macros: { public:
specifier }`), not merely unknown keys. The removed ones:
`extends`, `include_dirs`, `include_file`, `auto_objects`,
`driver_object`, top-level `create` (now `conventions.create`),
`profile_build.driver_config` (now `driver_config`),
`conventions.specifier_macros`, `conventions.format_registry` (sscanf is
built in), a rule's `max_per_file` (`--max-per-file` is the run's), and
`lint.fail_on` (`--fail-on` is the run's). Any other unknown key is an
error with the nearest real key suggested.

## 3. Projects and settings scopes

A `dgdtools.yml` is one of two things.

- A **root config** defines a project: it has a project key (`root`,
  `profile`, `lib_path`, `exclude`, `dialect`, ...). Its `root:` is the
  lib root. Without `root:` the root is never guessed: with `profile:`
  or `lib_path:` the config's directory is a checkout, and with neither
  the project has no lib and gets the file-local rules, with a note
  that says so (`root: .` makes the config's directory the lib root).
- A **settings scope** holds nothing but `lint:` and `format:`
  settings, or says `root: false`. It adjusts `lint:` and
  `format:` for the files under its directory, inside whatever project
  those files belong to. Any other key in it is an error naming the key
  (`usr/ann/dgdtools.yml: exclude belongs in the project's root
  config; this file is a settings scope (root: false) and holds only
  lint.rules, lint.overrides and format`), and so is `lint.baseline`,
  which describes a run, not a file. (`root:` is the lib
  root's directory in a root config; the one value that is not a
  directory, `false`, says there is none. A directory really called
  "false" is written `"false"`; `root: true` is an error.)

```yaml
# usr/ann/dgdtools.yml
root: false
format: {indent_style: tab}
lint:
  rules: {missing-visibility: warning}
  overrides:
    - paths: ["attic/**"]        # relative to this file's directory
      rules: {all: off}
```

**Finding the project of a path.** Walk up from the path; the first
`dgdtools.yml` that is not a settings scope is the project's root
config. A file of settings only is a scope when a root config is above
it; when none is, the outermost such file is the config of a project
without a lib, whose settings the files under it are formatted and
linted by. Scopes on the way are skipped, so files under different scopes
of one lib are one project, and "one project per invocation" (a lint run
has exactly one lib) is about root configs only.

**Finding the settings of a file.** The project's root config first,
then every `dgdtools.yml` between the project and the file, outermost
first. For each key the nearest file that sets it wins:

- `format:` keys replace.
- `lint.rules` entries are applied in order: the root's rules, the
  root's `overrides` that match, then each scope's rules and matching
  overrides. A scope is, in effect, an override that lives in the
  directory it is about. Globs in a scope's `overrides` are relative to
  the scope's directory; the root's are relative to the lib root.

A root config found *below* another project's (a wizard's own checkout
config, with `profile:` in it, sitting in the whole lib) is a project
boundary, for every command and for an editor alike: the files under it
belong to that project, whichever way they are come by. A file named by
itself, a document an editor opens and a file met in a walk all get the
project of the nearest root config above them. `dgdtools format DIR`
formats the files of a project below by that project's settings and
excludes; `dgdtools lint DIR`, whose run has one lib, passes over them
and says so in a note (`nested-project`), and they are linted by naming
that directory. The lib itself is not cut up by this: the index reads
every file under the lib root, so who inherits what is known whichever
project a file is linted in. (Reading such a file's `lint:` and
`format:` as if it were a scope, and ignoring the rest, made one file
mean two things depending on the command line; refusing it, as Biome
does, would make a whole-lib run fail because of a file one wizard
added.) `--config FILE` is the way to run one configuration over
everything below.

A scope that is not valid (a project key in it, a rule that does not
exist) is an error for the files under it, in `lint`, `format` and
`config` alike: they are not quietly handled by other settings.

`--config FILE` uses that file for every path and looks for no other
`dgdtools.yml` (ruff's rule: a file named by hand is the whole
configuration); `.editorconfig` is still read. `--no-config` uses no
configuration file of any kind, `.editorconfig` included.

**Where nested files are looked for.** From the file's directory up to
the project: below the directory of the project's config; or, when the
config names a lib somewhere else (`root: ../lib`), from the lib root
down. A file outside both has the project's own settings.

**A directory given to a command** stands for the files under it that
its project does not exclude, each formatted and linted by its own
settings as above; a project below it is treated as just said.

## 4. `.editorconfig`

For `format.indent_style`, `format.indent_width` and
`format.line_endings`, when no `dgdtools.yml` that applies to the file
sets the key, the file's `.editorconfig` properties are used:
`indent_style`; `indent_size` (or `tab_width` when it says `tab`);
`end_of_line` (`lf`, `crlf`). The standard algorithm: every
`.editorconfig` from the file's directory up to one with `root = true`,
sections matched against the file's path, nearer files and later
sections winning.

A `format:` key in any applicable `dgdtools.yml` beats `.editorconfig`
however near the latter is: the tool's own configuration is the more
specific statement (Prettier's rule). Nothing else is read from
`.editorconfig`; in particular `max_line_length` has no meaning here.

## 5. User-level configuration

`dgdtools.yml` in the user configuration directory
(`os.UserConfigDir()/dgdtools/`, else `~/.config/dgdtools/`) is used for
a path only when no project config is found for it. It is then that
path's root config, with one difference: it has no directory to be a lib
root, so a relative `root:` in it is an error. Its use is a wizard with
a bare checkout and no file in it: `profile: NAME` there, once.

It never merges with a project config. What a project's files are
formatted and linted by must not depend on who runs the tool. Settings
scopes above the path still apply over it, as over any root config, and
nothing is anchored at its directory (no default baseline).

## 6. Precedence

Highest first:

1. command-line flags;
2. the nearest settings scope that sets the key, then outer ones;
3. the project's root config (or `--config FILE`; or, when there is
   neither, the user-level config);
4. the profile (the driver's settings; the dialect and conventions the
   config leaves unset);
5. `.editorconfig`, for the three format keys;
6. built-in defaults.

`dgdtools config PATH` prints the project of a path and, for a file, its
resolved settings and where each came from. It is the tool's "verify":
it checks the configuration that applies to the path, and every
settings scope below a directory (a root config down there is named as
the project of its own it is), and exits 2 with the first thing that is
wrong.

An editor's own settings carry only what a wizard with no file at all
needs (a profile name, a lib path); they are the same fallback as the
user-level config and never override a project. They are per workspace
folder: `DiscoverOptions.Folders` names each folder they say something
of, and the paths under it that no config file claims are a project of
that folder's, anchored there (where the user-level config also applies,
the folder's profile and lib path are the nearer word).

## 7. In the library

Per-file resolution is not a command-line feature; the linter, the
formatter and the language server need the same answer, so it is in
`pkg/config`.

```go
opt := config.DiscoverOptions{WorkDir: wd, ProfileDirs: dirs, UserConfig: user, Format: flags}

prj, err := config.Discover(fs, paths, opt)   // the one project of some paths (lint, config)
ws := config.NewWorkspace(fs, opt)            // the project of any path (format, a server)
prj, err = ws.Project(path)

s, err := prj.Settings(path)   // *config.Settings
s.Format                       // every key set
s.From                         // where each key came from: a file, "command line", "default"
s.Scopes                       // the settings scopes that apply, outermost first (lint layers)
prj.Nested(path)               // the root config of the project below that path belongs to, or ""
prj.Inputs()                   // the files the project was resolved from

dropped, changed := ws.Invalidate(path)   // a configuration file changed
```

`Discover` and `Workspace` read no environment: the front end passes
the working directory, the profile directories and the user-level config
path (`config.UserConfigPath`). A `Project` caches what it reads per
directory and is safe for concurrent use. Under a long-lived server a
`Workspace` is told what changed (`session.Session` does it): a
`dgdtools.yml` drops the project it is the root config of and the
projects it is, or may now be, a settings scope of, and one of a
project's other inputs (its profile, the driver's configuration) drops
that project; each is resolved again, as a new
value, when next asked for. An `.editorconfig` is read again by every
project. `pkg/editorconfig` is the parser (sections, the glob forms of
the specification, `root = true`, `unset`).

`lint.New(prj, fs)` lints every file by `prj.Settings`, and
`Linter.EffectiveRules(path)` answers which rules run on a file;
`format.OptionsFor(prj.Config, s.Format)` are the formatter's options
for it.

## 8. The command

```
dgdtools lint [flags] [path ...]      lint files and directories (default: the project)
dgdtools format [flags] path ... | -  format files in place; --check, --diff
dgdtools rules                        list the lint rules
dgdtools explain RULE                 describe one rule
dgdtools config [path]                show the project and settings of a path
dgdtools profile build [-o FILE]      capture the whole lib as a profile
dgdtools profile show [NAME|FILE] [LIBPATH]
dgdtools version
```

- **One binary** (ruff, Biome, Deno, `go`): one thing to install, one
  version, and the name of the binary, the repository and the
  configuration file are the same word.
- **Subcommands, no default command.** `dgdtools PATH` is an error, so a
  file can never be mistaken for a command or the reverse.
- **`lint` and `format`**, the plain verbs; no aliases (`fmt`, `check`):
  one spelling in every script and every answer on a forum.
- **`lsp`** is the language server ([lsp.md](lsp.md)): the protocol on
  standard input and output, `--log FILE` and `--trace` for a log, exit
  code 1 when the editor went away without the protocol's shutdown.
- **`rename`** is the semantic rename and file move
  ([lsp.md](lsp.md) section 9), the same engine the editor's rename
  uses.
- **No token-comparison or token-dump command.** Comparing two files'
  significant tokens is the formatter's gate and runs on every file it
  formats; a token dump for lexer debugging is better done by a test or
  `go run` than by a hidden subcommand that has to be kept working.
  `pkg/tokcmp` is the library for anyone who needs the comparison.
- **Flags** belong to their subcommand and may stand before, between or
  after paths; `--` ends them. The flags that say where the project is
  (`--config`, `--no-config`, `--root`, `--profile`, `--lib-path`) are
  the same wherever a project is needed.
- **Exit codes**, every subcommand: 0 success; 1 the thing looked for
  was found (lint findings at or above `--fail-on`; files that
  `format --check` would change); 2 usage, configuration or I/O error.
- **Front ends are thin.** Each subcommand is
  `func(ctx, args, stdin, stdout, stderr) int` over the libraries: it
  parses flags, reads the working directory and environment, calls
  `pkg/config`, `pkg/lint`, `pkg/format`, `pkg/report`, and prints. No
  analysis, no layout and no configuration logic lives in `cmd/`. The
  one thing a front end does that a library must not is run another
  program: `format --changed` asks git for the changed lines and hands
  the library line ranges.
- **`lint --output-format`**, not `--format`: next to a subcommand
  called `format` the short name would be read wrong (ruff's choice
  too).
