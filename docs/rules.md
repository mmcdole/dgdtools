# Lint rules

<!-- Written from the rule table by `go test ./pkg/lint -run TestRulesDoc -update`; do not edit. -->

The rules of `dgdtools lint`, by group. `dgdtools rules` lists them and
`dgdtools explain RULE` prints one. A rule is set in `lint.rules` of
`dgdtools.yml` under its name or its code:

```yaml
lint:
  rules:
    unreachable-code: warning                    # off | info | warning | error
    lifecycle-chain: { names: [create, reset] }  # with options; severity is optional
```

"Needs the lib" says the rule reads other files than the one it reports
in: it runs when the lib root, or a profile of the lib, is known.

## syntax

The driver refuses to compile it: an object with one of these does not load.

### syntax-error

`E001` · error · on by default · one file is enough

The file does not parse; DGD will refuse to compile it.

Reported by the linter's own error-tolerant parser, which implements the DGD 1.7 grammar on unpreprocessed source. A missing ';' is reported where the ';' belongs (DGD reports the token after it), with a fix that inserts it when the parser went on as if it were there. Errors that could be an artifact of a macro the linter cannot see through are not reported: a statement that uses a fragment macro, or, when some #include could not be resolved, an error next to an unknown identifier. After an error that leaves the file's structure in doubt (unbalanced braces) later errors in the file are dropped as likely consequences.

### adjacent-strings

`E002` · error · on by default · one file is enough

Adjacent string literals ("a" "b"); DGD has no implicit concatenation.

Older LPmud drivers and C concatenate adjacent string literals. DGD does not: it is a syntax error and the object will not compile. Join the strings with '+' (DGD folds constant concatenation at compile time). The diagnostic carries a fix that inserts the '+'.

### legacy-closure

`E003` · error · on by default · one file is enough

(: ob, "fn" :) function-pointer literal; not DGD syntax.

The (: ... :) literal comes from other LPC dialects. DGD rejects it as a syntax error. Use the mudlib's function-pointer constructor instead.

### declaration-initializer

`E004` · error · on by default · one file is enough

Variable initialized in its declaration (int x = 5;); not DGD syntax.

DGD 1.7 declarations cannot carry an initializer, for globals or locals. It is a syntax error. Declare first and assign in a statement (globals: in create()).

### declaration-after-statement

`E005` · error · on by default · one file is enough

Local declaration after a statement in the same block; a DGD compile error.

DGD requires every local declaration of a block to come before the block's first statement (an empty statement ';' counts). Violations are the compile error "declaration after statement". Blocks in which a macro stands as a statement before the declaration are not reported, since the macro may expand to declarations.

### include-not-found

`E006` · error · on by default · needs the lib

#include target not found in the file's directory or the include dirs.

A missing #include is a compile error when the object loads. Only reported when the mudlib is complete (the configured include dirs and force-included header exist); in a partial checkout the target is unknown, not missing. Like a C compiler's fatal "file not found", it stops syntax reporting for the rest of the file.

### bad-directive

`E007` · error · on by default · one file is enough

Malformed preprocessor directive (text after #else/#endif, unknown directive).

DGD's preprocessor rejects tokens after #else and #endif ("bad token in control"; write them as a comment) and unknown directive names ("undefined control"), even inside a skipped #if block.

### inherit-after-declaration

`E008` · error · on by default · one file is enough

Inherit statement after a variable or function declaration.

DGD requires every inherit to precede all declarations ("inherit must precede all declarations"). That includes declarations that arrive through an #include placed before the inherit: a header with a prototype or a variable must be included after the inherit statements.

### missing-parameter-type

`E009` · error · on by default · one file is enough

Untyped parameter in a typeless function, under driver typechecking.

With typechecking >= 1 in the driver config, a function written without a return type is still type-checked, and a parameter without a type is the compile error "missing type for parameter". Silent when the typechecking level is unknown (set driver_config, or driver.typechecking).

### return-value

`E010` · error · on by default · one file is enough

Value returned from a void (or typeless) function, or bare return in a typed one.

DGD rejects 'return expr;' in a void function ("value returned from void function") and 'return;' in a function with a non-void type ("function must return value"). Under typechecking >= 1 a function declared without a return type is void, which makes this the most common reason old LPmud code does not compile on DGD: give the function a type.

## runtime

It compiles, and fails or silently returns nil when it runs.

### callable-not-found

`R001` · error · on by default · needs the lib

String-referenced function (obj->fn(), call_other, call_out, registrars) missing from the target's inherit chain.

In DGD a call_other to a function that does not exist silently returns nil. Reported only when the receiver is known (a string constant, a macro that evaluates to one, or this_object()) and the target's whole inherit chain was resolved and parsed; for a file that other objects inherit or #include, the function is also looked for in those users.

### static-crossobj

`R002` · error · on by default · needs the lib

Call_other to an unreachable function: private always, static from another object.

call_other cannot reach private functions at all, and reaches static functions only from the same object. The call silently returns nil. On this_object() a private function is reported only when the name is private in the whole chain and in every object that inherits the file, as private-callback has it: DGD keeps an inherited function that a private one redefines, and this_object() may be an inheritor that has it.

### undefined-prototype

`R003` · error · on by default · needs the lib

Function declared but never defined anywhere in the chain, and called.

DGD compiles a call to a function that only has a prototype and raises "Undefined function" when the call runs. Not reported when an inheritor or includer of the file provides the definition.

### target-object-missing

`R004` · error · on by default · needs the lib

Literal object path (inherit, clone_object, call target) with no backing file.

Loading an object whose file does not exist is a runtime error (for an inherit, a compile error). Paths served by virtual-object daemons belong in conventions.virtual_paths. Not reported when the mudlib on disk is incomplete.

### callback-arity

`R005` · warning · on by default · needs the lib

Dispatched call passing an argument count the target cannot accept.

Under non-strict typechecking DGD silently pads missing call_other arguments with nil and drops extras; under strict typechecking it is a runtime error.

### lifecycle-chain

`R006` · warning · on by default · needs the lib

Lifecycle function (the create function) does not call the inherited implementations.

A create function that overrides an inherited one without calling it (::create()) leaves the parent uninitialized. With several defining parents, label each inherit and chain each one. Proof-based: files with no explicit inherit, parents without the function, and unresolved parents are skipped. The function is the project's create function, conventions.create ("create" when it is not set; a mudlib whose auto object receives the driver's call and passes it on under another name sets the name its objects define). The option names lists other functions to hold to the same rule.

Options:

- `names`: a list of strings; the functions that must call what they override (default: the project's create function)

### private-callback

`R007` · error · on by default · needs the lib

Callback by name (call_out, a registrar of the lib) to a private function, which DGD never calls.

call_out("fn") and every callback the lib's registrars make by name (conventions.call_registry) find the function in the object's table of functions, and DGD leaves private functions out of that table, and the functions of an object inherited privately: the callback silently never happens. A static function is what such a callback can reach and no other object can call. Reported only when the receiver is this_object() and the name is a string constant, and when the name is private in the object's whole inherit chain, as known in full, and in every object that inherits or includes the file; not in a partial checkout or in the auto object. The ->fn and call_other forms are static-crossobj's. There is no fix: making the function static is what is usually meant, but a function that is no longer private redefines and clashes with the functions of that name of the objects it is inherited with, which only its author can weigh.

## suspicious

Legal, and almost certainly not what was meant.

### assignment-in-condition

`S001` · warning · on by default · one file is enough

Condition assigns a bare constant or the result of unparenthesized &&/||.

Assign-and-test (if (x = f())) is an accepted LPC idiom. Two forms are almost always bugs: assigning a constant (if (x = 0), a typo for ==) and storing the result of an unparenthesized logical expression (if (x = f() && ready) gives x 0 or 1). Parenthesizing the assignment or the right-hand side states the intent and silences the rule. Assignments with an invalid target (!x = f()) are compile errors and left to DGD.

### no-effect-statement

`S002` · warning · on by default · one file is enough

Comparison used as a statement (x == 1;).

A statement whose top-level operator is a comparison computes a value and discards it; almost always a typo for assignment.

### sscanf-format

`S003` · warning · on by default · one file is enough

More variables supplied than %-conversions in the scan format.

Fewer variables than conversions is the legal idiom for discarding trailing matches; more variables than conversions means some are never assigned. The function checked is sscanf.

### unreachable-code

`S004` · warning · off by default · one file is enough

Statement directly after return, break, continue or goto in the same block.

Code after an unconditional jump in the same block never runs. Labels and case labels restart reachability. Off by default: legal, and common as leftover debugging in old code.

### invalid-suppression

`S005` · warning · on by default · one file is enough

A dgdtools-ignore comment that does not do what it was written to do.

A comment that begins with dgdtools-ignore is an instruction, and one that cannot be followed protects nothing while looking as if it did: a rule name that does not exist (a typo), a target that is neither lint, lint/RULE nor format, a comment that names nothing, a range (dgdtools-ignore-start) that names no rule, an end with no start. The plain form is about one line: the line of code it stands on, or the next line when it stands alone; -start and -end are about what is between them; the formatter is turned off by range only. A comment that merely mentions dgdtools-ignore is not a directive.

## convention

The lib's own policy.

### raw-inherit-path

`C001` · warning · off by default · one file is enough

Inherit uses a literal path string where the mudlib mandates macros.

For mudlibs that name their inheritable objects with macros (inherit ROOM;), so that a moved file is one changed #define. The option deny restricts the rule to the parts of the mudlib that have such macros.

Options:

- `deny`: a list of strings; report only literal paths that begin with one of these (default: all of them)

### missing-visibility

`C002` · warning · off by default · one file is enough

Function with no visibility specifier.

Flags functions declared without private, static, nomask or one of the mudlib's specifier macros (a macro the file's headers define as empty or as specifiers, or one conventions.macros classes as specifier).

### unformatted

`C003` · info · off by default · one file is enough

File is not formatted as `dgdtools format` would format it.

Formats the file exactly as `dgdtools format` does, with the format settings of that file, and reports a difference. What dgdtools-ignore-start format comments protect is left alone here too. A file `dgdtools format` refuses to change (it does not tokenize, or the result could not be proven) is not reported: that command says why.

### static-autosave-var

`C004` · warning · off by default · one file is enough

Static global in an object that saves itself; excluded from save_object.

Static variables are not saved by save_object. Off by default: static is also the deliberate idiom for runtime-only state, so this is a review aid for specifier changes, not a standing audit. An object saves itself when its source mentions one of conventions.autosave_markers (the mudlib's function or macro that turns saving on); without markers the rule reports nothing.

### unresolved-inherit

`C005` · info · off by default · needs the lib

Inherit path the macro evaluator could not resolve.

Informational: cross-file rules cannot verify anything involving this object's inherit chain. Usually a path built from a function-like macro or a macro from an unresolved #include.

### unused-suppression

`C006` · warning · off by default · one file is enough

A dgdtools-ignore comment for a finding that is not there.

Reported for each rule a dgdtools-ignore comment names that has no finding where the comment says. A suppression that is not needed hides the next finding that turns up there. A comment for a rule that did not run on the file (it is off, or needs a mudlib that is not known) is not reported: whether it is needed cannot be told.
