package editors

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nvimSession is what the Neovim on PATH does with the language server:
// it opens a file of a lib, waits for its diagnostics, mends one finding
// by typing, another by the server's fix, has the file formatted, and
// writes down what it saw.
const nvimSession = `
local dir, out = vim.env.DGDLPC_DIR, {}
local ok, err = pcall(function()
  vim.opt.runtimepath:prepend(vim.env.DGDLPC_VIM)
  vim.cmd('filetype plugin indent on')
  vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp', '--log', dir .. '/lsp.log' } })
  vim.lsp.enable('dgdtools')
  vim.cmd('edit ' .. vim.fn.fnameescape(dir .. '/lib/obj/a.c'))
  out.filetype = vim.bo.filetype

  local function found()
    local list = {}
    for _, d in ipairs(vim.diagnostic.get(0)) do
      table.insert(list, string.format('%d:%d-%d %s %s', d.lnum, d.col, d.end_col, d.code, d.source))
    end
    table.sort(list)
    return list
  end
  local function wait(what, cond)
    assert(vim.wait(20000, cond, 20), 'waited in vain for ' .. what)
  end

  wait('diagnostics', function() return #found() == 2 end)
  out.first = found()
  local client = vim.lsp.get_clients({ bufnr = 0 })[1]
  out.encoding, out.root, out.server = client.offset_encoding, client.root_dir, client.server_info

  -- Typing: the comparison becomes an assignment, sent as a range.
  local row = 4
  local col = vim.api.nvim_buf_get_lines(0, row, row + 1, false)[1]:find('==', 1, true) - 1
  vim.api.nvim_buf_set_text(0, row, col, row, col + 2, { '=' })
  wait('one finding less', function() return #found() == 1 end)
  out.second = found()

  -- The server's fix for what is left.
  vim.lsp.buf.code_action({ context = { only = { 'source.fixAll' }, diagnostics = {} }, apply = true })
  wait('the fix', function() return vim.api.nvim_buf_get_lines(0, 5, 6, false)[1]:find('" + "', 1, true) ~= nil end)
  wait('no findings', function() return #found() == 0 end)

  -- Formatting, of a line that needs it.
  vim.api.nvim_buf_set_lines(0, 2, 3, false, { 'string     s;' })
  vim.lsp.buf.format({ timeout_ms = 10000 })
  out.text = vim.api.nvim_buf_get_lines(0, 0, -1, false)
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// nvimRun runs session, a Lua script, in the Neovim on PATH, headless,
// with the language server built from this tree and the files given
// below a new directory. The script finds in its environment the
// directory (DGDLPC_DIR), the server (DGDLPC_BIN) and the runtime
// directory editors/vim (DGDLPC_VIM), and leaves what it saw in
// result.json there, which is returned with the directory and the
// server's log. The test is skipped when there is no Neovim, or one too
// old to have vim.lsp.config (0.11).
func nvimRun(t *testing.T, files map[string]string, session string) (dir string, result []byte, log string) {
	t.Helper()
	return nvimRunAs(t, files, session, nvimStart{})
}

// nvimStart is how nvimRunAs starts Neovim. The zero value is bare (-u
// NONE: no plugins), and the session puts editors/vim on the runtime
// path itself.
type nvimStart struct {
	// As a user's Neovim starts with the plugin installed: --clean (the
	// built-in plugins, no user configuration), with editors/vim on the
	// runtime path before the plugins are loaded.
	plugins bool
	// Whether the server is found as dgdtools on PATH, which has nothing
	// else of the user's.
	onPath bool
	// Commands run before the plugins are loaded (--cmd).
	cmds []string
	// Files of the tree given on the command line, opened as Neovim
	// starts, before the session runs.
	args []string
}

// noEditor skips a test that needs an editor this machine does not have,
// and fails it where CI is set: CI installs the editors (ci.yml), so that
// a test that skips there does not pass unseen.
func noEditor(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("CI") != "" {
		t.Fatalf(format+" (CI is set: install the editor)", args...)
	}
	t.Skipf(format, args...)
}

func nvimRunAs(t *testing.T, files map[string]string, session string, start nvimStart) (dir string, result []byte, log string) {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the command and starts an editor")
	}
	nvim, err := exec.LookPath("nvim")
	if err != nil {
		noEditor(t, "no nvim on PATH")
	}
	if out, err := exec.Command(nvim, "--headless", "-u", "NONE", "-i", "NONE", "-n",
		"+lua io.stdout:write(tostring(vim.lsp.config ~= nil and vim.lsp.enable ~= nil))", "+qall!").Output(); err != nil || string(out) != "true" {
		noEditor(t, "the nvim on PATH has no vim.lsp.config (0.11 and later): %s %v", out, err)
	}
	dir, err = filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	bin := filepath.Join(dir, "bin", "dgdtools")
	if out, err := exec.Command("go", "build", "-o", bin, "../cmd/dgdtools").CombinedOutput(); err != nil {
		t.Fatalf("building the command: %v\n%s", err, out)
	}
	for rel, text := range files {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), text)
	}
	writeFile(t, filepath.Join(dir, "session.lua"), session)
	vimDir, err := filepath.Abs(vimDir)
	require.NoError(t, err)

	args := []string{"--headless", "-u", "NONE", "-i", "NONE", "-n"}
	env := append(os.Environ(), "DGDLPC_DIR="+dir, "DGDLPC_BIN="+bin, "DGDLPC_VIM="+vimDir)
	if start.plugins {
		args = []string{"--headless", "--clean", "-n", "--cmd", "set rtp^=" + vimDir}
		path := "/usr/bin:/bin"
		if start.onPath {
			path = filepath.Dir(bin) + ":" + path
		}
		env = append(env, "PATH="+path)
	}
	for _, c := range start.cmds {
		args = append(args, "--cmd", c)
	}
	for _, a := range start.args {
		args = append(args, filepath.Join(dir, filepath.FromSlash(a)))
	}
	cmd := exec.Command(nvim, append(args, "-S", filepath.Join(dir, "session.lua"))...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	result, err = os.ReadFile(filepath.Join(dir, "result.json"))
	require.NoError(t, err, "%s", out)
	data, _ := os.ReadFile(filepath.Join(dir, "lsp.log"))
	return dir, result, string(data)
}

// TestNeovimClient runs the language server under the Neovim on PATH,
// with the configuration and the runtime files in editors/vim: the built-in client of Neovim 0.11 and later is the
// first editor the server is used from.
func TestNeovimClient(t *testing.T) {
	const text = "void f()\n{\n    string s;\n\n    /* blåbær \U0001F600 */ s == \"x\";\n    s = \"one\" \"two\";\n}\n"
	dir, data, log := nvimRun(t, map[string]string{"lib/dgdtools.yml": "root: .\n", "lib/obj/a.c": text}, nvimSession)
	var got struct {
		Error, Filetype, Encoding, Root string
		Server                          struct{ Name string }
		First, Second, Text             []string
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "the server's log:\n%s", log)

	assert.Equal(t, "dgdlpc", got.Filetype)
	assert.Equal(t, "dgdtools", got.Server.Name)
	assert.Equal(t, filepath.Join(dir, "lib"), got.Root)
	// Neovim counts in bytes when the server lets it, which it does.
	assert.Equal(t, "utf-8", got.Encoding)
	col := strings.Index(strings.Split(text, "\n")[4], "==")
	require.Equal(t, 26, col, "bytes, where UTF-16 units would be 22")
	assert.Equal(t, []string{"4:26-28 S002 dgdtools", "5:14-19 E002 dgdtools"}, got.First)
	assert.Equal(t, []string{"5:14-19 E002 dgdtools"}, got.Second)
	assert.Equal(t, strings.Split("void f()\n{\n    string s;\n\n    /* blåbær \U0001F600 */ s = \"x\";\n    s = \"one\" + \"two\";\n}", "\n"), got.Text)
	assert.Contains(t, log, "msg=initialized encoding=utf-8")
}

// nvimDefinitions is Neovim going to definitions: with the cursor on a
// name it asks for the definition as a user does (vim.lsp.buf.definition,
// what CTRL-] does through 'tagfunc'), and says where it ended up.
const nvimDefinitions = `
local dir, out = vim.env.DGDLPC_DIR, { went = {} }
local ok, err = pcall(function()
  vim.opt.runtimepath:prepend(vim.env.DGDLPC_VIM)
  vim.cmd('filetype plugin indent on')
  vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp', '--log', dir .. '/lsp.log' } })
  vim.lsp.enable('dgdtools')
  local a = dir .. '/lib/obj/a.c'
  vim.cmd('edit ' .. vim.fn.fnameescape(a))
  local function wait(what, cond)
    assert(vim.wait(20000, cond, 20), 'waited in vain for ' .. what)
  end
  wait('the server', function()
    local c = vim.lsp.get_clients({ bufnr = 0 })[1]
    return c ~= nil and c.initialized
  end)
  out.provider = vim.lsp.get_clients({ bufnr = 0 })[1].server_capabilities.definitionProvider

  -- From the first place in a.c that has the text, with the cursor so
  -- many bytes into it, to wherever the definition is.
  for _, at in ipairs(vim.json.decode(vim.env.DGDLPC_PLACES)) do
    vim.cmd('edit ' .. vim.fn.fnameescape(a))
    local text, into = at[1], at[2]
    local row, col
    for i, line in ipairs(vim.api.nvim_buf_get_lines(0, 0, -1, false)) do
      local found = line:find(text, 1, true)
      if found then row, col = i, found - 1 + into break end
    end
    assert(row, 'no ' .. text)
    vim.api.nvim_win_set_cursor(0, { row, col })
    local from = { vim.api.nvim_buf_get_name(0), row, col }
    vim.lsp.buf.definition()
    local function here()
      local pos = vim.api.nvim_win_get_cursor(0)
      return { vim.api.nvim_buf_get_name(0), pos[1], pos[2] }
    end
    vim.wait(at[3] and 500 or 20000, function() return not vim.deep_equal(here(), from) end, 20)
    local h = here()
    local line = vim.api.nvim_buf_get_lines(0, h[2] - 1, h[2], false)[1]
    table.insert(out.went, string.format('%s -> %s:%d %s', text, h[1]:sub(#dir + 2), h[2], line:sub(h[3] + 1)))
  end
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// TestNeovimDefinition has the Neovim on PATH go to definitions through
// the language server: into another file, into a header, within the
// file, with characters of several bytes before the names on both sides.
func TestNeovimDefinition(t *testing.T) {
	files := map[string]string{
		"lib/dgdtools.yml":  "root: .\ndriver_config: mud.dgd\n",
		"lib/mud.dgd":       "include_dirs = ({ \"/include\" });\n",
		"lib/include/std.h": "/* blåbær \U0001F600 */ #define BASE \"/std/base\"\n",
		"lib/std/base.c":    "int weight;\n/* grøt */ void create() {}\n",
		"lib/obj/a.c": "#include <std.h>\ninherit BASE;\nvoid later();\n" +
			"void create() {\n    /* først \U0001F600 */ ::create(); weight = 1; call_out(\"later\", 1); sizeof(({ }));\n}\nvoid later() {}\n",
	}
	// The text to find, how far into it the cursor is, and whether
	// nothing is to happen.
	t.Setenv("DGDLPC_PLACES", `[["::create", 3], ["weight", 0], ["inherit BASE", 9], ["<std.h>", 2], ["\"later\"", 2], ["later();", 1], ["sizeof", 1, true]]`)
	_, data, log := nvimRun(t, files, nvimDefinitions)
	var got struct {
		Error    string
		Provider bool
		Went     []string
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "the server's log:\n%s", log)
	assert.True(t, got.Provider)
	assert.Equal(t, []string{
		"::create -> lib/std/base.c:2 create() {}",
		"weight -> lib/std/base.c:1 weight;",
		"inherit BASE -> lib/std/base.c:1 int weight;",
		"<std.h> -> lib/include/std.h:1 /* blåbær \U0001F600 */ #define BASE \"/std/base\"",
		"\"later\" -> lib/obj/a.c:7 later() {}",
		"later(); -> lib/obj/a.c:7 later() {}",
		"sizeof -> lib/obj/a.c:5 izeof(({ }));",
	}, got.Went)
	assert.NotContains(t, log, "level=ERROR")
}

// nvimHovers is Neovim asking about places as a user does: with the
// cursor on a name it asks for the hover (vim.lsp.buf.hover, what K
// does), and inside the arguments of a call for the signature
// (vim.lsp.buf.signature_help, CTRL-S in insert mode), and writes down
// what the window that opens says, and the parameter it marks.
const nvimHovers = `
local dir, out = vim.env.DGDLPC_DIR, { shown = {} }
local ok, err = pcall(function()
  vim.opt.runtimepath:prepend(vim.env.DGDLPC_VIM)
  vim.cmd('filetype plugin indent on')
  vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp', '--log', dir .. '/lsp.log' } })
  vim.lsp.enable('dgdtools')
  vim.cmd('edit ' .. vim.fn.fnameescape(dir .. '/lib/obj/a.c'))
  local function wait(what, cond)
    assert(vim.wait(20000, cond, 20), 'waited in vain for ' .. what)
  end
  wait('the server', function()
    local c = vim.lsp.get_clients({ bufnr = 0 })[1]
    return c ~= nil and c.initialized
  end)
  local caps = vim.lsp.get_clients({ bufnr = 0 })[1].server_capabilities
  out.hover, out.triggers = caps.hoverProvider, caps.signatureHelpProvider.triggerCharacters

  -- The window that floats over the text, if there is one.
  local function float()
    for _, win in ipairs(vim.api.nvim_list_wins()) do
      if vim.api.nvim_win_get_config(win).relative ~= '' then return win end
    end
  end
  local function close()
    local win = float()
    while win do
      vim.api.nvim_win_close(win, true)
      win = float()
    end
  end

  -- At the first place in the file that has the text, with the cursor so
  -- many bytes into it: what the hover or the signature shows, and the
  -- text marked as the parameter being written.
  for _, at in ipairs(vim.json.decode(vim.env.DGDLPC_PLACES)) do
    local what, text, into, nothing = at[1], at[2], at[3], at[4]
    local row, col
    for i, line in ipairs(vim.api.nvim_buf_get_lines(0, 0, -1, false)) do
      local found = line:find(text, 1, true)
      if found then row, col = i, found - 1 + into break end
    end
    assert(row, 'no ' .. text)
    vim.api.nvim_win_set_cursor(0, { row, col })
    if what == 'hover' then vim.lsp.buf.hover() else vim.lsp.buf.signature_help() end
    vim.wait(nothing and 500 or 20000, function() return float() ~= nil end, 20)
    local shown = { what = what, at = text }
    local win = float()
    if win then
      local buf = vim.api.nvim_win_get_buf(win)
      shown.lines = vim.api.nvim_buf_get_lines(buf, 0, -1, false)
      for _, mark in ipairs(vim.api.nvim_buf_get_extmarks(buf, -1, 0, -1, { details = true })) do
        local d = mark[4]
        if d.hl_group == 'LspSignatureActiveParameter' and d.end_col then
          shown.active = vim.api.nvim_buf_get_text(buf, mark[2], mark[3], d.end_row or mark[2], d.end_col, {})[1]
        end
      end
    end
    table.insert(out.shown, shown)
    close()
  end
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// TestNeovimHover has the Neovim on PATH show hovers and signatures
// from the language server: a function of another file with the comment
// above it, a kfun, a macro, a call whose receiver nobody knows, and the
// arguments of calls, with characters of several bytes before the names.
func TestNeovimHover(t *testing.T) {
	files := map[string]string{
		"lib/dgdtools.yml":  "root: .\ndriver_config: mud.dgd\n",
		"lib/mud.dgd":       "include_dirs = ({ \"/include\" });\n",
		"lib/include/std.h": "/* blåbær \U0001F600 */ #define BASE \"/std/base\"\n",
		"lib/std/base.c":    "int weight;\n/* Sets the weight,\n   in grams. */\nvarargs void set_weight(int grams, string why...) {}\nvoid create() {}\n",
		"lib/obj/a.c": "#include <std.h>\ninherit BASE;\n" +
			"void create() {\n    /* først \U0001F600 */ ::create(); set_weight(1, \"a\", \"b\"); weight = strlen(\"x\"); ob->set_weight(2); nope(3);\n}\n",
	}
	// What is asked, the text to find, how far into it the cursor is,
	// and whether nothing is to show.
	t.Setenv("DGDLPC_PLACES", `[["hover", "set_weight(1", 3], ["hover", "strlen", 2], ["hover", "inherit BASE", 9], ["hover", "weight =", 1],
		["hover", "ob->set_weight", 6], ["hover", "nope", 1, true],
		["signature", "set_weight(1", 11], ["signature", "\"b\"", 1], ["signature", "strlen(", 7],
		["signature", "ob->set_weight(2", 15, true], ["signature", "nope(3", 5, true]]`)
	_, data, log := nvimRun(t, files, nvimHovers)
	var got struct {
		Error    string
		Hover    bool
		Triggers []string
		Shown    []struct {
			What, At, Active string
			Lines            []string
		}
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "the server's log:\n%s", log)
	assert.True(t, got.Hover)
	assert.Equal(t, []string{"(", ","}, got.Triggers)
	// What each window says, its lines joined; Neovim shows the Markdown
	// as it reads it, and what it does with the marks is its own.
	var shown []string
	for _, s := range got.Shown {
		line := s.What + " " + s.At + ":"
		for _, want := range []string{
			"varargs void set_weight(int grams, string why...)", "Sets the weight,", "in grams.", "/std/base.c:4",
			"int strlen(string)", "return the length of a string", "kfun, DGD 1.7.9",
			"/std/base", "int weight", "/std/base.c:1", "the receiver is not known",
		} {
			if strings.Contains(strings.Join(s.Lines, "\n"), want) {
				line += " <" + want + ">"
			}
		}
		if s.Active != "" {
			line += " [" + s.Active + "]"
		}
		if s.Lines == nil {
			line += " nothing"
		}
		shown = append(shown, line)
	}
	assert.Equal(t, []string{
		"hover set_weight(1: <varargs void set_weight(int grams, string why...)> <Sets the weight,> <in grams.> </std/base.c:4> </std/base>",
		"hover strlen: <int strlen(string)> <return the length of a string> <kfun, DGD 1.7.9>",
		"hover inherit BASE: </std/base>",
		"hover weight =: </std/base> <int weight> </std/base.c:1>",
		"hover ob->set_weight: <the receiver is not known>",
		"hover nope: nothing",
		"signature set_weight(1: <varargs void set_weight(int grams, string why...)> <Sets the weight,> <in grams.> </std/base.c:4> </std/base> [int grams]",
		"signature \"b\": <varargs void set_weight(int grams, string why...)> <Sets the weight,> <in grams.> </std/base.c:4> </std/base> [string why...]",
		"signature strlen(: <int strlen(string)> <return the length of a string> <kfun, DGD 1.7.9> [string]",
		"signature ob->set_weight(2: nothing",
		"signature nope(3: nothing",
	}, shown)
	assert.NotContains(t, log, "level=ERROR")
}

// nvimSymbols is Neovim listing symbols as a user does: the outline of
// the buffer (vim.lsp.buf.document_symbol, gO) and the names of the
// workspace that match a query (vim.lsp.buf.workspace_symbol), each as
// the list Neovim makes of the answer.
const nvimSymbols = `
local dir, out = vim.env.DGDLPC_DIR, { outline = {}, found = {} }
local ok, err = pcall(function()
  vim.opt.runtimepath:prepend(vim.env.DGDLPC_VIM)
  vim.cmd('filetype plugin indent on')
  vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp', '--log', dir .. '/lsp.log' } })
  vim.lsp.enable('dgdtools')
  vim.cmd('edit ' .. vim.fn.fnameescape(dir .. '/lib/obj/a.c'))
  local function wait(what, cond)
    assert(vim.wait(20000, cond, 20), 'waited in vain for ' .. what)
  end
  wait('the server', function()
    local c = vim.lsp.get_clients({ bufnr = 0 })[1]
    return c ~= nil and c.initialized
  end)
  local caps = vim.lsp.get_clients({ bufnr = 0 })[1].server_capabilities
  out.document, out.workspace = caps.documentSymbolProvider, caps.workspaceSymbolProvider

  -- Neovim works out the column of a name in another file from the
  -- line of that number in this one: only the outline's are told.
  local function list(into, cols)
    local done = false
    return function(o)
      for _, item in ipairs(o.items) do
        local at = cols and string.format('%d:%d', item.lnum, item.col) or tostring(item.lnum)
        table.insert(into, string.format('%s:%s %s', item.filename:sub(#dir + 2), at, item.text))
      end
      done = true
    end, function() return done end
  end
  local on_list, done = list(out.outline, true)
  vim.lsp.buf.document_symbol({ on_list = on_list })
  wait('the outline', done)
  for _, query in ipairs({ 'weight', 'BASE' }) do
    on_list, done = list(out.found)
    vim.lsp.buf.workspace_symbol(query, { on_list = on_list })
    wait('the symbols for ' .. query, done)
  end
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// TestNeovimSymbols has the Neovim on PATH list the outline of a file
// and search the lib for names through the language server, with
// characters of several bytes before the names.
func TestNeovimSymbols(t *testing.T) {
	files := map[string]string{
		"lib/dgdtools.yml":  "root: .\ndriver_config: mud.dgd\n",
		"lib/mud.dgd":       "include_dirs = ({ \"/include\" });\n",
		"lib/include/std.h": "/* blåbær \U0001F600 */ #define BASE \"/std/base\"\n",
		"lib/std/base.c":    "/* grøt */ int weight;\nvarargs void set_weight(int grams, string why...) {}\nvoid create() {}\n",
		"lib/obj/a.c": "#include <std.h>\n#define TWICE(x) ((x) + (x))\ninherit BASE;\nprivate int count;\n" +
			"/* først \U0001F600 */ void create() { ::create(); set_weight(TWICE(1)); }\n",
	}
	_, data, log := nvimRun(t, files, nvimSymbols)
	var got struct {
		Error               string
		Document, Workspace bool
		Outline, Found      []string
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "the server's log:\n%s", log)
	assert.True(t, got.Document)
	assert.True(t, got.Workspace)
	// Neovim's columns are 1-based bytes, and it says the kind in words.
	assert.Equal(t, []string{
		"lib/obj/a.c:2:9 [Constant] TWICE",
		"lib/obj/a.c:3:9 [Class] /std/base",
		"lib/obj/a.c:4:13 [Variable] count",
		"lib/obj/a.c:5:24 [Function] create",
	}, got.Outline)
	assert.Equal(t, []string{
		"lib/std/base.c:1 [Variable] weight in /std/base",
		"lib/std/base.c:2 [Function] set_weight in /std/base",
		"lib/include/std.h:1 [Constant] BASE in /include/std.h",
	}, got.Found)
	assert.NotContains(t, log, "level=ERROR")
}

// nvimCompletions is Neovim completing as a user does: in insert mode
// it types something on a line of a function's body and asks for
// completions with CTRL-X CTRL-O (the omnifunc, which the client sets to
// the server's), and writes down what the menu offers and what the line
// is once the first offer is taken.
const nvimCompletions = `
local dir, out = vim.env.DGDLPC_DIR, { cases = {} }
local ok, err = pcall(function()
  vim.opt.runtimepath:prepend(vim.env.DGDLPC_VIM)
  vim.cmd('filetype plugin indent on')
  vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp', '--log', dir .. '/lsp.log' } })
  vim.lsp.enable('dgdtools')
  vim.cmd('edit ' .. vim.fn.fnameescape(dir .. '/lib/obj/a.c'))
  local function wait(what, cond)
    assert(vim.wait(20000, cond, 20), 'waited in vain for ' .. what)
  end
  wait('the server', function()
    local c = vim.lsp.get_clients({ bufnr = 0 })[1]
    return c ~= nil and c.initialized
  end)
  local caps = vim.lsp.get_clients({ bufnr = 0 })[1].server_capabilities
  out.triggers, out.resolve = caps.completionProvider.triggerCharacters, caps.completionProvider.resolveProvider
  out.omnifunc = vim.bo.omnifunc
  vim.opt.completeopt = { 'menu', 'menuone', 'noselect' }

  -- In insert mode, with the menu asked for: what it has, when it comes.
  local seen
  vim.keymap.set('i', '<F5>', function()
    vim.wait(seen.nothing and 700 or 20000, function() return vim.fn.pumvisible() == 1 end, 20)
    for _, item in ipairs(vim.fn.complete_info({ 'items' }).items or {}) do
      table.insert(seen.words, item.word)
      seen.kinds[item.word] = item.kind
      seen.menus[item.word] = item.menu
    end
  end)
  local row
  for i, line in ipairs(vim.api.nvim_buf_get_lines(0, 0, -1, false)) do
    if line:find('HERE', 1, true) then row = i end
  end
  for _, case in ipairs(vim.json.decode(vim.env.DGDLPC_PLACES)) do
    seen = { typed = case[1], words = {}, kinds = vim.empty_dict(), menus = vim.empty_dict(), nothing = case[2] }
    vim.api.nvim_win_set_cursor(0, { row, 0 })
    -- The line is written anew, the menu asked for and looked at, its
    -- first offer taken when there is to be one.
    local keys = 'S' .. case[1] .. '<C-x><C-o><F5>' .. (case[2] and '' or '<C-n><C-y>') .. '<Esc>'
    vim.api.nvim_feedkeys(vim.keycode(keys), 'x', false)
    seen.line = vim.api.nvim_buf_get_lines(0, row - 1, row, false)[1]
    table.insert(out.cases, seen)
  end
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// TestNeovimCompletion has the Neovim on PATH complete through the
// language server, with characters of several bytes before the cursor:
// a name in code, the functions after "::" and after "->" on a path, a
// callback's name in its string, the header of an #include; and nothing
// on a receiver nobody knows.
func TestNeovimCompletion(t *testing.T) {
	files := map[string]string{
		"lib/dgdtools.yml":  "root: .\ndriver_config: mud.dgd\n",
		"lib/mud.dgd":       "include_dirs = ({ \"/include\" });\n",
		"lib/include/std.h": "/* blåbær \U0001F600 */ #define BASE \"/std/base\"\n",
		"lib/std/base.c":    "int weight;\nvarargs void set_weight(int grams, string why...) {}\nvoid create() {}\n",
		"lib/obj/a.c": "#include <std.h>\ninherit BASE;\nvoid later_on(int when) {}\n" +
			"void create(object ob) {\n    int local_count;\n    HERE\n}\n",
	}
	// What is typed on the line, and whether no menu is to come.
	t.Setenv("DGDLPC_PLACES", `[["/* blåbær 😀 */ local_", false], ["later", false], ["::cr", false], ["\"/std/base\"->set", false],
		["call_out(\"lat", false], ["#include <st", false], ["ob->", true]]`)
	_, data, log := nvimRun(t, files, nvimCompletions)
	var got struct {
		Error    string
		Triggers []string
		Resolve  bool
		Omnifunc string
		Cases    []struct {
			Typed, Line  string
			Words        []string
			Kinds, Menus map[string]string
		}
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "the server's log:\n%s", log)
	assert.Equal(t, []string{">", ":", "\"", "/", "<"}, got.Triggers)
	assert.True(t, got.Resolve)
	assert.Equal(t, "v:lua.vim.lsp.omnifunc", got.Omnifunc)
	var seen []string
	for _, c := range got.Cases {
		line := c.Typed + " => " + strings.TrimSpace(c.Line) + " [" + strings.Join(c.Words, " ") + "]"
		if len(c.Words) > 0 {
			line += " " + c.Kinds[c.Words[0]] + " " + c.Menus[c.Words[0]]
		}
		seen = append(seen, line)
	}
	// Neovim filters what the server offers by what is written, takes
	// an item's text for the word, and says its kind and its detail.
	assert.Equal(t, []string{
		"/* blåbær \U0001F600 */ local_ => /* blåbær \U0001F600 */ local_count [local_count] Variable int local_count",
		"later => later_on [later_on] Function void later_on(int when)",
		"::cr => ::create [create] Function void create()",
		"\"/std/base\"->set => \"/std/base\"->set_weight [set_weight] Function varargs void set_weight(int grams, string why...)",
		"call_out(\"lat => call_out(\"later_on [later_on] Function void later_on(int when)",
		"#include <st => #include <std.h [std.h] File ",
		"ob-> => ob-> []",
	}, seen)
	assert.NotContains(t, log, "level=ERROR")
}

// nvimReferences is Neovim finding references as a user does: with the
// cursor on a name it asks for them (vim.lsp.buf.references, what grr
// does) and for the highlights of the name (vim.lsp.buf.document_highlight),
// each as Neovim lays out the answer.
const nvimReferences = `
local dir, out = vim.env.DGDLPC_DIR, { found = {} }
local ok, err = pcall(function()
  vim.opt.runtimepath:prepend(vim.env.DGDLPC_VIM)
  vim.cmd('filetype plugin indent on')
  vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp', '--log', dir .. '/lsp.log' } })
  vim.lsp.enable('dgdtools')
  vim.cmd('edit ' .. vim.fn.fnameescape(dir .. '/lib/obj/a.c'))
  local function wait(what, cond)
    assert(vim.wait(20000, cond, 20), 'waited in vain for ' .. what)
  end
  wait('the server', function()
    local c = vim.lsp.get_clients({ bufnr = 0 })[1]
    return c ~= nil and c.initialized
  end)
  local caps = vim.lsp.get_clients({ bufnr = 0 })[1].server_capabilities
  out.references, out.highlight = caps.referencesProvider, caps.documentHighlightProvider

  local function cursor(text, into)
    for i, line in ipairs(vim.api.nvim_buf_get_lines(0, 0, -1, false)) do
      local found = line:find(text, 1, true)
      if found then
        vim.api.nvim_win_set_cursor(0, { i, found - 1 + into })
        return
      end
    end
    error('no ' .. text)
  end
  for _, at in ipairs({ { 'set_weight(1', 3 }, { 'count =', 2 } }) do
    cursor(at[1], at[2])
    local done = false
    vim.lsp.buf.references({ includeDeclaration = true }, { on_list = function(o)
      for _, item in ipairs(o.items) do
        table.insert(out.found, string.format('%s %s:%d:%d %s', at[1], item.filename:sub(#dir + 2), item.lnum, item.col, vim.trim(item.text)))
      end
      done = true
    end })
    wait('the references of ' .. at[1], function() return done end)
  end

  -- The highlights of the local where the cursor is, as extmarks.
  cursor('count =', 2)
  vim.lsp.buf.document_highlight()
  local ns = vim.api.nvim_get_namespaces()['nvim.lsp.references'] or vim.api.nvim_get_namespaces()['vim_lsp_references']
  wait('the highlights', function()
    ns = ns or vim.api.nvim_get_namespaces()['nvim.lsp.references'] or vim.api.nvim_get_namespaces()['vim_lsp_references']
    return ns ~= nil and #vim.api.nvim_buf_get_extmarks(0, ns, 0, -1, {}) > 0
  end)
  out.marks = {}
  for _, m in ipairs(vim.api.nvim_buf_get_extmarks(0, ns, 0, -1, { details = true })) do
    table.insert(out.marks, string.format('%d:%d-%d %s', m[2], m[3], m[4].end_col, m[4].hl_group))
  end
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// TestNeovimReferences has the Neovim on PATH find the references of a
// function across the lib and of a local in its function, and highlight
// the local read and written, with characters of several bytes before
// the names.
func TestNeovimReferences(t *testing.T) {
	files := map[string]string{
		"lib/dgdtools.yml":  "root: .\ndriver_config: mud.dgd\n",
		"lib/mud.dgd":       "include_dirs = ({ \"/include\" });\n",
		"lib/include/std.h": "#define BASE \"/std/base\"\n",
		"lib/std/base.c":    "/* grøt */ void set_weight(int grams) {}\n",
		"lib/obj/b.c":       "#include <std.h>\ninherit BASE;\nvoid reset() { set_weight(2); }\n",
		"lib/obj/a.c": "#include <std.h>\ninherit BASE;\n" +
			"void create() {\n    int count;\n    /* først \U0001F600 */ set_weight(1); count = 1; count++;\n    set_weight(count);\n}\n",
	}
	_, data, log := nvimRun(t, files, nvimReferences)
	var got struct {
		Error                 string
		References, Highlight bool
		Found, Marks          []string
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "the server's log:\n%s", log)
	assert.True(t, got.References)
	assert.True(t, got.Highlight)
	// Neovim's columns are 1-based bytes.
	assert.Equal(t, []string{
		"set_weight(1 lib/obj/a.c:5:23 /* først \U0001F600 */ set_weight(1); count = 1; count++;",
		"set_weight(1 lib/obj/a.c:6:5 set_weight(count);",
		"set_weight(1 lib/obj/b.c:3:16 void reset() { set_weight(2); }",
		"set_weight(1 lib/std/base.c:1:18 /* grøt */ void set_weight(int grams) {}",
		"count = lib/obj/a.c:4:9 int count;",
		"count = lib/obj/a.c:5:38 /* først \U0001F600 */ set_weight(1); count = 1; count++;",
		"count = lib/obj/a.c:5:49 /* først \U0001F600 */ set_weight(1); count = 1; count++;",
		"count = lib/obj/a.c:6:16 set_weight(count);",
	}, got.Found)
	// Written, written, written, read: 0-based rows and byte columns.
	assert.Equal(t, []string{
		"3:8-13 LspReferenceWrite",
		"4:37-42 LspReferenceWrite",
		"4:48-53 LspReferenceWrite",
		"5:15-20 LspReferenceRead",
	}, got.Marks)
	assert.NotContains(t, log, "level=ERROR")
}

// nvimTokens has the Neovim on PATH show the semantic tokens of a file,
// before and after an edit (a delta), and writes down what it has at
// some names.
const nvimTokens = `
local dir, out = vim.env.DGDLPC_DIR, { before = {}, after = {} }
local ok, err = pcall(function()
  vim.opt.runtimepath:prepend(vim.env.DGDLPC_VIM)
  vim.cmd('filetype plugin indent on')
  vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp', '--log', dir .. '/lsp.log' } })
  vim.lsp.enable('dgdtools')
  vim.cmd('edit ' .. vim.fn.fnameescape(dir .. '/lib/obj/a.c'))
  local function wait(what, cond)
    assert(vim.wait(20000, cond, 20), 'waited in vain for ' .. what)
  end
  local function at(text)
    for i, line in ipairs(vim.api.nvim_buf_get_lines(0, 0, -1, false)) do
      local found = line:find(text, 1, true)
      if found then
        local list = {}
        for _, tk in ipairs(vim.lsp.semantic_tokens.get_at_pos(0, i - 1, found - 1) or {}) do
          local mods = {}
          for m in pairs(tk.modifiers) do table.insert(mods, m) end
          table.sort(mods)
          table.insert(list, tk.type .. (#mods > 0 and (' ' .. table.concat(mods, ',')) or ''))
        end
        return text .. ': ' .. table.concat(list, ' ')
      end
    end
    return text .. ': no such text'
  end
  wait('the tokens', function() return at('set_weight(1') ~= 'set_weight(1: ' end)
  local caps = vim.lsp.get_clients({ bufnr = 0 })[1].server_capabilities
  out.delta = caps.semanticTokensProvider.full.delta
  for _, text in ipairs({ 'BASE', 'set_weight(1', 'count;', 'count = ', 'heavy', 'grams' }) do
    table.insert(out.before, at(text))
  end
  -- An edit: count becomes a global's name; the client asks for a delta.
  vim.api.nvim_buf_set_lines(0, 3, 4, false, { '' })
  wait('the tokens after the edit', function() return at('count = ') == 'count = : ' end)
  table.insert(out.after, at('count = '))
  table.insert(out.after, at('set_weight(1'))
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// TestNeovimTokens has the Neovim on PATH highlight a file with the
// server's semantic tokens, which its built-in client asks for by
// itself (0.11 and later), in full and then as a delta after an edit.
func TestNeovimTokens(t *testing.T) {
	files := map[string]string{
		"lib/dgdtools.yml":  "root: .\ndriver_config: mud.dgd\n",
		"lib/mud.dgd":       "include_dirs = ({ \"/include\" });\n",
		"lib/include/std.h": "#define BASE \"/std/base\"\n",
		"lib/std/base.c":    "/* grøt */ void set_weight(int grams) {}\n",
		"lib/obj/a.c": "#include <std.h>\ninherit BASE;\n" +
			"static int heavy(int grams) {\n    int count;\n    /* først \U0001F600 */ set_weight(1); count = grams;\n    return count;\n}\n",
	}
	_, data, log := nvimRun(t, files, nvimTokens)
	var got struct {
		Error         string
		Delta         bool
		Before, After []string
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "the server's log:\n%s", log)
	assert.True(t, got.Delta)
	assert.Equal(t, []string{
		"BASE: macro",
		"set_weight(1: function inherited",
		"count;: variable declaration,local",
		"count = : variable local",
		"heavy: function definition,static",
		"grams: parameter declaration",
	}, got.Before)
	// The local is gone: the name is nothing the tools know.
	assert.Equal(t, []string{"count = : ", "set_weight(1: function inherited"}, got.After)
	assert.Contains(t, log, "method=textDocument/semanticTokens/full/delta")
	assert.NotContains(t, log, "level=ERROR")
}

// nvimRename is Neovim renaming as a user does: with the cursor on a
// name, vim.lsp.buf.rename (what grn does) asks the server whether it
// can be renamed and then for the edits, and makes them; where it cannot,
// Neovim shows the server's reason.
const nvimRename = `
local dir, out = vim.env.DGDLPC_DIR, { said = {} }
local ok, err = pcall(function()
  vim.opt.runtimepath:prepend(vim.env.DGDLPC_VIM)
  vim.cmd('filetype plugin indent on')
  vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp', '--log', dir .. '/lsp.log' } })
  vim.lsp.enable('dgdtools')
  vim.cmd('edit ' .. vim.fn.fnameescape(dir .. '/lib/obj/a.c'))
  local function wait(what, cond)
    assert(vim.wait(20000, cond, 20), 'waited in vain for ' .. what)
  end
  wait('the server', function()
    local c = vim.lsp.get_clients({ bufnr = 0 })[1]
    return c ~= nil and c.initialized
  end)
  out.prepare = vim.lsp.get_clients({ bufnr = 0 })[1].server_capabilities.renameProvider.prepareProvider
  vim.notify = function(msg) table.insert(out.said, msg) end
  local function cursor(text, into)
    for i, line in ipairs(vim.api.nvim_buf_get_lines(0, 0, -1, false)) do
      local found = line:find(text, 1, true)
      if found then
        vim.api.nvim_win_set_cursor(0, { i, found - 1 + into })
        return
      end
    end
    error('no ' .. text)
  end
  local function text() return table.concat(vim.api.nvim_buf_get_lines(0, 0, -1, false), '\n') end

  local before = text()
  cursor('count = 1', 2)
  vim.lsp.buf.rename('total')
  wait('the rename of count', function() return text() ~= before end)
  before = text()
  cursor('private void help', 14)
  vim.lsp.buf.rename('assist')
  wait('the rename of helper', function() return text() ~= before end)
  before = text()
  cursor('void greet', 7)
  vim.lsp.buf.rename('welcome')
  wait('the rename of greet', function() return text() ~= before end)
  local other = vim.fn.bufnr(dir .. '/lib/obj/b.c')
  wait('the edit of the other file', function()
    return other ~= -1 and table.concat(vim.api.nvim_buf_get_lines(other, 0, -1, false), '\n'):find('welcome') ~= nil
  end)
  out.other = vim.api.nvim_buf_get_lines(other, 0, -1, false)
  cursor('void create', 6)
  vim.lsp.buf.rename('make')
  wait('the reason', function() return #out.said > 0 end)
  out.text = vim.api.nvim_buf_get_lines(0, 0, -1, false)
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// TestNeovimRename has the Neovim on PATH rename a local and a private
// function, with characters of several bytes before the names, and a
// public function, which another file calls; and be told why the create
// function is not renamed.
func TestNeovimRename(t *testing.T) {
	files := map[string]string{
		"lib/dgdtools.yml": "root: .\ndriver_config: mud.dgd\n",
		"lib/mud.dgd":      "include_dirs = ({ \"/include\" });\n",
		"lib/obj/a.c": "private void helper(int n) {}\n" +
			"void create() {\n    int count;\n    /* først \U0001F600 */ helper(1); count = 1; count++;\n    helper(count);\n}\n" +
			"void greet() {}\n",
		"lib/obj/b.c": "void f() {\n    /* þ */ \"/obj/a\"->greet();\n}\n",
	}
	_, data, log := nvimRun(t, files, nvimRename)
	var got struct {
		Error   string
		Prepare bool
		Said    []string
		Text    []string
		Other   []string
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "the server's log:\n%s", log)
	assert.True(t, got.Prepare)
	assert.Equal(t, []string{
		"private void assist(int n) {}",
		"void create() {",
		"    int total;",
		"    /* først \U0001F600 */ assist(1); total = 1; total++;",
		"    assist(total);",
		"}",
		"void welcome() {}",
	}, got.Text)
	assert.Equal(t, []string{"void f() {", "    /* þ */ \"/obj/a\"->welcome();", "}"}, got.Other)
	require.Len(t, got.Said, 1)
	assert.Contains(t, got.Said[0], "'create' is the function the driver calls by name to initialize every object (create in the configuration)")
	assert.NotContains(t, log, "level=ERROR")
}

// nvimRefactor is Neovim with the refactorings: the prototype of a
// function called before its definition, by vim.lsp.buf.code_action
// (what gra does) applied at once; and a file moved as a file explorer
// plugin moves one (oil.nvim, nvim-tree): workspace/willRenameFiles, its
// edit applied, then vim.lsp.util.rename.
const nvimRefactor = `
local dir, out = vim.env.DGDLPC_DIR, {}
local ok, err = pcall(function()
  vim.opt.runtimepath:prepend(vim.env.DGDLPC_VIM)
  vim.cmd('filetype plugin indent on')
  vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp', '--log', dir .. '/lsp.log' } })
  vim.lsp.enable('dgdtools')
  vim.cmd('edit ' .. vim.fn.fnameescape(dir .. '/lib/obj/a.c'))
  local function wait(what, cond)
    assert(vim.wait(20000, cond, 20), 'waited in vain for ' .. what)
  end
  local client
  wait('the server', function()
    client = vim.lsp.get_clients({ bufnr = 0 })[1]
    return client ~= nil and client.initialized
  end)
  local function text(b) return vim.api.nvim_buf_get_lines(b or 0, 0, -1, false) end
  vim.api.nvim_win_set_cursor(0, { 2, 12 })
  local before = table.concat(text(), '\n')
  vim.lsp.buf.code_action({ filter = function(a) return a.kind == 'refactor.rewrite' end, apply = true })
  wait('the prototype', function() return table.concat(text(), '\n') ~= before end)
  out.text = text()

  local old, new = dir .. '/lib/std/room.c', dir .. '/lib/std/place.c'
  local res = client:request_sync('workspace/willRenameFiles',
    { files = { { oldUri = vim.uri_from_fname(old), newUri = vim.uri_from_fname(new) } } }, 20000)
  assert(res and not res.err, vim.inspect(res))
  vim.lsp.util.apply_workspace_edit(res.result, client.offset_encoding)
  vim.lsp.util.rename(old, new)
  out.other = text(vim.fn.bufnr(dir .. '/lib/obj/a.c'))
  out.exists = vim.uv.fs_stat(new) ~= nil
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// TestNeovimRefactor has the Neovim on PATH add a prototype with a code
// action, and move a file the current one inherits, the inherit edited.
func TestNeovimRefactor(t *testing.T) {
	files := map[string]string{
		"lib/dgdtools.yml": "root: .\n",
		"lib/std/room.c":   "int light;\n",
		"lib/obj/a.c":      "inherit \"/std/room\";\nvoid f() { helper(1); }\n\nint helper(int n) { return n; }\n",
	}
	dir, data, log := nvimRun(t, files, nvimRefactor)
	var got struct {
		Error  string
		Text   []string
		Other  []string
		Exists bool
	}
	require.NoError(t, json.Unmarshal(data, &got), "%s", data)
	require.Empty(t, got.Error, "the server's log:\n%s", log)
	assert.Equal(t, []string{"inherit \"/std/room\";", "int helper(int n);", "", "void f() { helper(1); }", "", "int helper(int n) { return n; }"}, got.Text)
	assert.Equal(t, []string{"inherit \"/std/place\";", "int helper(int n);", "", "void f() { helper(1); }", "", "int helper(int n) { return n; }"}, got.Other)
	assert.True(t, got.Exists)
	assert.NoFileExists(t, filepath.Join(dir, "lib/std/room.c"))
	assert.NotContains(t, log, "level=ERROR")
}

// nvimPlugin is Neovim started as a user starts it with the plugin
// installed and nothing configured: it opens a file of a lib and says
// whether the language server was started for it, by what command.
const nvimPlugin = `
local dir, out = vim.env.DGDLPC_DIR, {}
local ok, err = pcall(function()
  if vim.env.DGDLPC_LATE == '1' then
    vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp' } })
  end
  vim.cmd('edit ' .. vim.fn.fnameescape(dir .. '/lib/obj/a.c'))
  out.filetype = vim.bo.filetype
  vim.wait(vim.env.DGDLPC_WAIT == '1' and 20000 or 1000, function()
    local c = vim.lsp.get_clients({ bufnr = 0 })[1]
    return c ~= nil and c.initialized
  end, 20)
  local c = vim.lsp.get_clients({ bufnr = 0 })[1]
  out.started = c ~= nil and c.name == 'dgdtools' and c.initialized
  out.errmsg = vim.v.errmsg
end)
if not ok then out.error = tostring(err) end
vim.fn.writefile({ vim.json.encode(out) }, dir .. '/result.json')
vim.cmd('qall!')
`

// TestNeovimPlugin starts Neovim with editors/vim as a plugin manager
// installs it: plugin/dgdtools.lua enables the server when its command
// can be run, the one on PATH or the one configured, also after plugins
// have loaded, and not when g:dgdtools_lsp is false (or 0, as a vimrc
// says it). The file is opened by the session, and also on the command
// line, where its filetype is set before Neovim has started.
func TestNeovimPlugin(t *testing.T) {
	files := map[string]string{"lib/dgdtools.yml": "root: .\n", "lib/obj/a.c": "void create() {}\n"}
	configured := "lua vim.lsp.config('dgdtools', { cmd = { vim.env.DGDLPC_BIN, 'lsp' } })"
	for _, c := range []struct {
		name    string
		start   nvimStart
		late    bool
		started bool
	}{
		{"on PATH", nvimStart{onPath: true}, false, true},
		{"on PATH, a file on the command line", nvimStart{onPath: true, args: []string{"lib/obj/a.c"}}, false, true},
		{"configured", nvimStart{cmds: []string{configured}}, false, true},
		{"configured after plugins load", nvimStart{}, true, true},
		{"not found", nvimStart{}, false, false},
		{"opted out", nvimStart{onPath: true, cmds: []string{"lua vim.g.dgdtools_lsp = false"}}, false, false},
		{"opted out in a vimrc", nvimStart{onPath: true, cmds: []string{"let g:dgdtools_lsp = 0"}}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.start.plugins = true
			wait := "0"
			if c.started {
				wait = "1"
			}
			t.Setenv("DGDLPC_WAIT", wait)
			late := "0"
			if c.late {
				late = "1"
			}
			t.Setenv("DGDLPC_LATE", late)
			_, data, _ := nvimRunAs(t, files, nvimPlugin, c.start)
			var got struct {
				Error, Filetype, Errmsg string
				Started                 bool
			}
			require.NoError(t, json.Unmarshal(data, &got), "%s", data)
			require.Empty(t, got.Error)
			assert.Equal(t, "dgdlpc", got.Filetype)
			assert.Equal(t, c.started, got.Started)
			assert.Empty(t, got.Errmsg)
		})
	}
}
