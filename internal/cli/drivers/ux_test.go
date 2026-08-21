package drivers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/cache"
	"fxvcs.dev/fxvcs/internal/cli"
	"fxvcs.dev/fxvcs/internal/gitclient/gittest"
)

// driverEnv makes the test binary behave as the fxvcs CLI when Git launches
// it as the filter/hook launcher: os.Executable() is the launcher.
const driverEnv = "FXVCS_TEST_DRIVER"

func TestMain(m *testing.M) {
	if os.Getenv(driverEnv) == "1" {
		args := append([]string{"fxvcs"}, os.Args[1:]...)
		env := cli.Env{Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Extra: Commands()}
		os.Exit(cli.Run(context.Background(), env, args))
	}
	os.Exit(m.Run())
}

// bannedTokens must never appear in default human output (docs/cli-ux.md §2,
// §6.4). They are allowed under --verbose and in --json.
var bannedTokens = []string{"pointer", "ledger", "index entry", "spec.", "storageDomain", "repositoryID", "app:", "filter=fxvcs", "&{", "map[", "hydrat", "%!"}

// A stack is a bundle of resource repositories that can be deployed to any
// number of servers; a target is one server. Calling a stack a "server setup"
// (or any other server word) collapses that distinction and teaches the wrong
// model, so stack output may not use server nouns for itself.
var bannedStackTokens = []string{"server setup", "your server's", "this server's", "server config"}

func assertStackVocabulary(t *testing.T, label, text string) {
	t.Helper()
	for _, tok := range bannedStackTokens {
		if strings.Contains(strings.ToLower(text), tok) {
			t.Errorf("%s: %q calls a stack a server:\n%s", label, tok, text)
		}
	}
}

type uxFixture struct {
	repo  *gittest.Repo
	exe   string
	store string
}

func newUXFixture(t *testing.T) *uxFixture {
	t.Helper()
	gittest.RequireGit(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	r := gittest.InitRepo(t, driverEnv+"=1", cache.EnvDir+"="+cacheDir)
	for _, kv := range r.Env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "USERPROFILE", "XDG_CONFIG_HOME", "GIT_CONFIG_NOSYSTEM", "GIT_CONFIG_GLOBAL",
			"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL",
			"GIT_AUTHOR_DATE", "GIT_COMMITTER_DATE", driverEnv, cache.EnvDir:
			t.Setenv(k, v)
		}
	}
	t.Setenv("XDG_DATA_HOME", filepath.Join(t.TempDir(), "xdg"))
	t.Setenv("LOCALAPPDATA", filepath.Join(t.TempDir(), "local"))
	t.Setenv("NO_COLOR", "1")
	return &uxFixture{repo: r, exe: exe, store: t.TempDir()}
}

type runResult struct {
	code   int
	stdout string
	stderr string
}

func (f *uxFixture) run(t *testing.T, tty bool, args ...string) runResult {
	t.Helper()
	var out, errb bytes.Buffer
	env := cli.Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Extra: Commands(),
		Getwd: func() (string, error) { return f.repo.Root, nil }}
	if tty {
		env.IsTerminal = func(io.Writer) bool { return true }
	}
	code := cli.Run(context.Background(), env, append([]string{"fxvcs"}, args...))
	return runResult{code, out.String(), errb.String()}
}

func (r runResult) all() string { return r.stdout + r.stderr }

func assertClean(t *testing.T, label, text string) {
	t.Helper()
	for _, tok := range bannedTokens {
		if strings.Contains(text, tok) {
			t.Errorf("%s: banned token %q in human output:\n%s", label, tok, text)
		}
	}
}

func TestHumanOutputVocabularyAndShapes(t *testing.T) {
	f := newUXFixture(t)
	f.repo.WriteFile("stream/city.ytd", bytes.Repeat([]byte("city"), 600_000))         // 2.3 MiB
	f.repo.WriteFile("stream/city_props.ytyp", bytes.Repeat([]byte("p"), 18_000))      // 17.6 KiB
	f.repo.WriteFile("stream/bank_int.ymap", bytes.Repeat([]byte("bank"), 375_000))    // 1.4 MiB
	f.repo.WriteFile("big.bin", bytes.Repeat([]byte("b"), 2_000_000))                  // untracked large file
	f.repo.WriteFile("fxmanifest.lua", []byte("fx_version 'cerulean'\ngame 'gta5'\n")) // makes it a single resource

	// Before init: not a repository.
	r := f.run(t, false, "status")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "error: this folder is not an fxvcs repository") {
		t.Fatalf("status before init: %+v", r)
	}
	assertClean(t, "status-before-init", r.all())

	r = f.run(t, false, "init", "--profile", "fivem", "--launcher", f.exe)
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "fxvcs init — set up") || !strings.Contains(r.stdout, "FiveM repository") || !strings.Contains(r.stdout, "git add .fxvcs .gitattributes") {
		t.Fatalf("init: %+v", r)
	}
	assertClean(t, "init", r.all())

	// Same-command re-run: plain cause + hint, exit 1.
	r = f.run(t, false, "init", "--profile", "fivem", "--launcher", f.exe)
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "error: this repository is already set up for fxvcs\n  hint: fxvcs setup") {
		t.Fatalf("second init: %+v", r)
	}

	// The fivem profile presets track *.ytd/*.ytyp/*.ymap; git add ingests.
	f.repo.Git("add", ".")
	f.repo.Git("commit", "-q", "-m", "init")

	r = f.run(t, false, "status")
	if r.code != cli.ExitOK {
		t.Fatalf("status: %+v", r)
	}
	for _, want := range []string{"fxvcs status — 3 tracked assets in this repository", "loaded  stream/city.ytd", "2.3 MiB", "not yet uploaded", "1 large file is not tracked by fxvcs — see fxvcs scan"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("status lacks %q:\n%s", want, r.stdout)
		}
	}
	assertClean(t, "status", r.all())

	// Publish without storage: plain cause naming the fix.
	r = f.run(t, false, "objects", "publish")
	if r.code != cli.ExitUsage || !strings.Contains(r.stderr, "error: no shared storage is set up for this repository") || !strings.Contains(r.stderr, "hint: fxvcs storage add --name primary") {
		t.Fatalf("publish without storage: %+v", r)
	}
	assertClean(t, "publish-no-storage", r.all())

	// Declare a committed remote but do not bind it: unbound error names the remote.
	cfg := f.repo.ReadFile(".fxvcs/repository.yaml")
	cfg = append(cfg, []byte("  storage:\n    remotes:\n      - name: primary\n        type: filesystem\n        requiredForPublication: true\n")...)
	cfg = bytes.Replace(cfg, []byte("  storage:\n    remotes: []\n"), nil, 1)
	f.repo.WriteFile(".fxvcs/repository.yaml", cfg)
	r = f.run(t, false, "objects", "publish")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "error: storage \"primary\" is not set up on this computer") || !strings.Contains(r.stderr, "hint: fxvcs storage add --name primary --path <folder shared with your team>") {
		t.Fatalf("publish unbound: %+v", r)
	}
	assertClean(t, "publish-unbound", r.all())
	r = f.run(t, false, "storage", "list")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "not set up") || !strings.Contains(r.stdout, "hint: fxvcs storage add --name primary") {
		t.Fatalf("storage list unbound: %+v", r)
	}
	assertClean(t, "storage-list", r.all())

	r = f.run(t, false, "storage", "add", "--name", "primary", "--path", f.store)
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "storage \"primary\" is set up on this computer") {
		t.Fatalf("storage add: %+v", r)
	}
	assertClean(t, "storage-add", r.all())

	// Publish off a terminal: one line per finished *asset* on stderr (not per
	// stored chunk), footer on stdout. The uploaded byte count is below the
	// asset sizes because chunking deduplicates repeated regions.
	r = f.run(t, false, "objects", "publish", "--concurrency", "2")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "fxvcs objects publish — uploaded 3 assets (2.7 MiB) in 7 pieces to storage primary in") || strings.Count(r.stderr, "uploaded  stream/") != 3 {
		t.Fatalf("publish: %+v", r)
	}
	assertClean(t, "publish", r.all())
	r = f.run(t, false, "objects", "publish")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "nothing to upload; storage already has every asset") {
		t.Fatalf("publish again: %+v", r)
	}

	// Unload/load: per-item rows, refusals exit 0, nothing-done exits 1.
	r = f.run(t, false, "unload")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "fxvcs unload — 3 assets requested") || !strings.Contains(r.stdout, "3 unloaded.") {
		t.Fatalf("unload: %+v", r)
	}
	assertClean(t, "unload", r.all())
	r = f.run(t, false, "status")
	if !strings.Contains(r.stdout, "placeholder  stream/city.ytd") || !strings.Contains(r.stdout, "(fxvcs load stream/city.ytd)") || !strings.Contains(r.stdout, "3 not loaded (fxvcs load)") {
		t.Fatalf("status placeholders: %+v", r)
	}
	assertClean(t, "status-placeholders", r.all())
	r = f.run(t, false, "load", "stream/city.ytd", "nope.bin")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "loaded   stream/city.ytd") || !strings.Contains(r.stdout, "skipped  nope.bin") || !strings.Contains(r.stdout, "not tracked by fxvcs (fxvcs track nope.bin to start tracking it)") || !strings.Contains(r.stdout, "1 loaded, 1 skipped.") {
		t.Fatalf("load partial: %+v", r)
	}
	if !strings.Contains(r.stderr, "loaded  stream/city.ytd") {
		t.Fatalf("off-terminal progress line missing: %q", r.stderr)
	}
	assertClean(t, "load-partial", r.all())
	r = f.run(t, false, "load", "nope.bin")
	if r.code != cli.ExitFailure || !strings.Contains(r.stderr, "error: nothing could be done: nope.bin: not tracked by fxvcs") || !strings.Contains(r.stderr, "hint: fxvcs track nope.bin") {
		t.Fatalf("load nothing: %+v", r)
	}
	assertClean(t, "load-nothing", r.all())
	// The hidden aliases keep working and are not advertised.
	r = f.run(t, false, "hydrate", "stream/city_props.ytyp")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "fxvcs load — 1 asset requested") {
		t.Fatalf("hydrate alias: %+v", r)
	}
	r = f.run(t, false, "download", "--help")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "fxvcs load") {
		t.Fatalf("download alias help: %+v", r)
	}

	// A locally changed asset is refused (exit 0) and reported as such.
	f.repo.WriteFile("stream/city.ytd", bytes.Repeat([]byte("edit"), 600_001))
	r = f.run(t, false, "unload")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "skipped") || !strings.Contains(r.stdout, "stream/city.ytd") || !strings.Contains(r.stdout, "changed locally") {
		t.Fatalf("unload dirty: %+v", r)
	}
	assertClean(t, "unload-dirty", r.all())
	r = f.run(t, false, "status")
	if !strings.Contains(r.stdout, "changed locally") || !strings.Contains(r.stdout, "1 asset changed locally: commit with git add/commit") {
		t.Fatalf("status dirty: %+v", r)
	}
	assertClean(t, "status-dirty", r.all())

	// Diagnostics.
	for _, args := range [][]string{{"verify"}, {"verify", "--remote"}, {"doctor"}, {"scan"}, {"objects", "status"}, {"cache", "status"}, {"cache", "prune"}, {"storage", "test", "primary"}, {"tracking", "check", "stream/city.ytd"}, {"tracking", "check", "big.bin"}, {"upgrade"}, {"upgrade", "--check"}, {"compat"}, {"resources", "sync"}, {"resources", "validate"}, {"info"}, {"version"}, {"setup", "--launcher", f.exe}, {"track", "--glob", "*.ydr"}, {"untrack", "--glob", "*.ydr"}} {
		r = f.run(t, false, args...)
		if r.code == cli.ExitUsage {
			t.Errorf("%v: usage error %+v", args, r)
		}
		if strings.Contains(r.stderr, "panic") {
			t.Errorf("%v: panic %+v", args, r)
		}
		label := strings.Join(args, " ")
		if label != "info" { // info is where ids are allowed
			assertClean(t, label, r.all())
		}
		if r.stdout != "" && !strings.HasPrefix(r.stdout, "fxvcs ") {
			t.Errorf("%v: no header line:\n%s", args, r.stdout)
		}
	}
	r = f.run(t, false, "migrate", "big.bin")
	if r.code != cli.ExitUsage || !strings.Contains(r.stderr, "error: big.bin is not tracked by fxvcs") || !strings.Contains(r.stderr, "hint: fxvcs track big.bin") {
		t.Fatalf("migrate untracked: %+v", r)
	}
	assertClean(t, "migrate-untracked", r.all())
	r = f.run(t, false, "doctor")
	if r.code != cli.ExitFailure || !strings.Contains(r.stdout, "these need your decision:") || !strings.Contains(r.stdout, "stream/city.ytd") {
		t.Fatalf("doctor dirty: %+v", r)
	}
	r = f.run(t, false, "info")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "repository id") || !strings.Contains(r.stdout, "storage domain id") || !strings.Contains(r.stdout, "primary") {
		t.Fatalf("info: %+v", r)
	}
	r = f.run(t, false, "objects", "status")
	if !strings.Contains(r.stdout, "3 uploaded, 0 not yet uploaded") {
		t.Fatalf("objects status: %+v", r)
	}
	r = f.run(t, false, "scan")
	if !strings.Contains(r.stdout, "big.bin") || !strings.Contains(r.stdout, "not tracked") {
		t.Fatalf("scan: %+v", r)
	}

	// Help text of every command is clean too.
	root := cli.New(cli.Env{Extra: Commands()})
	for _, c := range root.Commands {
		if c.Hidden {
			continue
		}
		names := [][]string{{c.Name}}
		for _, sub := range c.Commands {
			if sub.Name != "help" {
				names = append(names, []string{c.Name, sub.Name})
			}
		}
		for _, n := range names {
			r = f.run(t, false, append(n, "--help")...)
			if r.code != cli.ExitOK {
				t.Errorf("help %v: %+v", n, r)
			}
			if c.Name != "spec" && c.Name != "fivem" { // driver diagnostics keep their technical vocabulary
				assertClean(t, "help "+strings.Join(n, " "), r.stdout)
			}
		}
	}

	// --quiet prints nothing on success and only the error on failure.
	r = f.run(t, false, "--quiet", "status")
	if r.code != cli.ExitOK || r.stdout != "" || r.stderr != "" {
		t.Fatalf("quiet status: %+v", r)
	}
	r = f.run(t, false, "--quiet", "load", "nope.bin")
	if r.code != cli.ExitFailure || r.stdout != "" || !strings.HasPrefix(r.stderr, "error: ") {
		t.Fatalf("quiet failure: %+v", r)
	}

	// --json is unchanged: raw states, codes, and details.
	r = f.run(t, false, "--json", "status")
	var st map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &st); err != nil || st["entries"] == nil {
		t.Fatalf("json status: %v %+v", err, r)
	}
	if !strings.Contains(r.stdout, "hydrated-modified") {
		t.Fatalf("json states must stay raw: %s", r.stdout)
	}
	r = f.run(t, false, "--json", "load", "nope.bin")
	var payload map[string]map[string]any
	if err := json.Unmarshal([]byte(r.stderr), &payload); err != nil || payload["error"]["code"] != "not-found" || payload["error"]["hint"] == nil {
		t.Fatalf("json error payload: %v %s", err, r.stderr)
	}
}

func TestPublishLiveProgressOnTerminal(t *testing.T) {
	f := newUXFixture(t)
	f.repo.WriteFile("a.ytd", bytes.Repeat([]byte("a"), 300_000))
	f.repo.WriteFile("b.ytd", bytes.Repeat([]byte("b"), 200_000))
	if r := f.run(t, false, "init", "--launcher", f.exe, "--remote-name", "primary", "--remote-path", f.store); r.code != cli.ExitOK {
		t.Fatalf("init: %+v", r)
	}
	if r := f.run(t, false, "track", "--glob", "*.ytd"); r.code != cli.ExitOK {
		t.Fatalf("track: %+v", r)
	}
	f.repo.Git("add", ".")
	f.repo.Git("commit", "-q", "-m", "init")

	// Forced terminal: progress goes to stderr as a live region that is torn
	// down again, and the result goes to stdout. What the region contains is
	// the live model's contract and is tested there
	// (internal/cli/ui/live), deterministically; asserting on frames a
	// renderer emitted on its own schedule would only test the scheduler.
	r := f.run(t, true, "--color=always", "objects", "publish")
	if r.code != cli.ExitOK {
		t.Fatalf("publish: %+v", r)
	}
	if !strings.Contains(r.stderr, "\x1b[") {
		t.Errorf("no live region was drawn on a terminal:\n%q", r.stderr)
	}
	// The live region must leave the terminal as it found it: cursor shown
	// again, and no leftover text on stderr for the shell prompt to collide
	// with.
	if !strings.Contains(r.stderr, "\x1b[?25h") {
		t.Errorf("live region did not restore the cursor:\n%q", r.stderr)
	}
	if !strings.Contains(r.stdout, "uploaded 2 assets") {
		t.Errorf("stdout: %q", r.stdout)
	}
	// Progress never contaminates the machine-readable stream.
	if strings.Contains(r.stdout, "\x1b[?25l") {
		t.Errorf("live control sequences leaked onto stdout: %q", r.stdout)
	}
	// --quiet suppresses progress entirely.
	f.repo.WriteFile("c.ytd", bytes.Repeat([]byte("c"), 100_000))
	f.repo.Git("add", ".")
	r = f.run(t, true, "--quiet", "objects", "publish")
	if r.code != cli.ExitOK || r.stderr != "" || r.stdout != "" {
		t.Fatalf("quiet publish: %+v", r)
	}
	// Refusal only, exit 0: unload on placeholders is "already unloaded".
	f.repo.WriteFile("d.ytd", bytes.Repeat([]byte("d"), 100_000))
	f.repo.Git("add", ".")
	f.repo.Git("commit", "-q", "-m", "more")
	r = f.run(t, false, "unload")
	if r.code != cli.ExitOK {
		t.Fatalf("unload: %+v", r)
	}
	r = f.run(t, false, "unload")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "already unloaded") || !strings.Contains(r.stdout, "0 unloaded, 4 skipped.") {
		t.Fatalf("unload again: %+v", r)
	}
	assertClean(t, "unload-again", r.all())
}

// TestCoordinationOutputVocabulary walks the Stage 3 commands the way an
// operator does — one component repository, one stack, a release, an
// environment, and a server — and holds their human output to the same
// vocabulary rules as the rest of the CLI. A stack is a deployable unit, not a
// server: the two nouns must not be used for each other.
func TestCoordinationOutputVocabulary(t *testing.T) {
	f := newUXFixture(t)

	// A component repository with two resources, published to a remote.
	f.repo.WriteFile("city-map/fxmanifest.lua", []byte("fx_version 'cerulean'\ngame 'gta5'\n"))
	f.repo.WriteFile("bank/fxmanifest.lua", []byte("fx_version 'cerulean'\ngame 'gta5'\n"))
	if r := f.run(t, false, "init", "--profile", "fivem", "--name", "maps", "--launcher", f.exe); r.code != cli.ExitOK {
		t.Fatalf("init: %+v", r)
	}
	if r := f.run(t, false, "resources", "sync", "--new", "enabled"); r.code != cli.ExitOK {
		t.Fatalf("resources sync: %+v", r)
	}
	f.repo.Git("add", ".")
	f.repo.Git("commit", "-q", "-m", "init")
	remote := gittest.BareRemote(t)
	f.repo.Git("remote", "add", "origin", remote)
	f.repo.Git("push", "-q", "origin", "HEAD:refs/heads/main")

	// The stack lives in its own Git repository.
	server := gittest.InitRepo(t, driverEnv+"=1")
	runIn := func(t *testing.T, args ...string) runResult {
		t.Helper()
		var out, errb bytes.Buffer
		env := cli.Env{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Extra: Commands(),
			Getwd: func() (string, error) { return server.Root, nil }}
		code := cli.Run(context.Background(), env, append([]string{"fxvcs"}, args...))
		return runResult{code, out.String(), errb.String()}
	}

	r := runIn(t, "stack", "init", "--name", "my-server")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, `created the stack "my-server"`) {
		t.Fatalf("stack init: %+v", r)
	}
	assertClean(t, "stack init", r.all())
	assertStackVocabulary(t, "stack init", r.all())

	r = runIn(t, "stack", "add", "file://"+remote)
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "installs at resources/[maps]") {
		t.Fatalf("stack add: %+v", r)
	}
	assertClean(t, "stack add", r.all())
	assertStackVocabulary(t, "stack add", r.all())

	r = runIn(t, "stack", "status")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "1 resource repository") {
		t.Fatalf("stack status: %+v", r)
	}
	assertClean(t, "stack status", r.all())
	assertStackVocabulary(t, "stack status", r.all())

	r = runIn(t, "stack", "validate")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "ready for a release") {
		t.Fatalf("stack validate: %+v", r)
	}
	assertClean(t, "stack validate", r.all())
	assertStackVocabulary(t, "stack validate", r.all())

	r = runIn(t, "release", "create", "v1.0.0")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "v1.0.0 written") {
		t.Fatalf("release create: %+v", r)
	}
	assertClean(t, "release create", r.all())
	assertStackVocabulary(t, "release create", r.all())

	// A second release under the same name with different content is refused
	// in plain language.
	f.repo.WriteFile("bank/server.lua", []byte("-- more\n"))
	f.repo.Git("add", ".")
	f.repo.Git("commit", "-q", "-m", "change")
	f.repo.Git("push", "-q", "origin", "HEAD:refs/heads/main")
	if r := runIn(t, "stack", "sync"); r.code != cli.ExitOK {
		t.Fatalf("stack sync: %+v", r)
	}
	r = runIn(t, "release", "create", "v1.0.0")
	if r.code == cli.ExitOK || !strings.Contains(r.stderr, "already exists with different content") {
		t.Fatalf("overwrite refusal: %+v", r)
	}
	if !strings.Contains(r.stderr, "hint: pick a new release name") {
		t.Fatalf("refusal without a next step: %+v", r)
	}

	r = runIn(t, "release", "inspect", "v1.0.0", "--all")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "2 resources start") {
		t.Fatalf("release inspect: %+v", r)
	}
	assertClean(t, "release inspect", r.all())

	r = runIn(t, "environment", "set", "prd", "--input", "hostname=Example")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "prd created (revision 1)") {
		t.Fatalf("environment set: %+v", r)
	}
	assertClean(t, "environment set", r.all())

	r = runIn(t, "target", "set", "prd", "--release", "v1.0.0", "--environment", "prd",
		"--agent", "agent-a", "--runtime", "fxserver-main", "--secret-provider", "envfile")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, `created server "prd", set to v1.0.0`) {
		t.Fatalf("target set: %+v", r)
	}
	assertClean(t, "target set", r.all())

	r = runIn(t, "release", "list")
	if r.code != cli.ExitOK || !strings.Contains(r.stdout, "running on prd") {
		t.Fatalf("release list: %+v", r)
	}
	assertClean(t, "release list", r.all())

	// The notes draft is the output, not a report about the output.
	r = runIn(t, "release", "notes", "generate", "v1.0.0")
	if r.code != cli.ExitOK || !strings.HasPrefix(r.stdout, "# v1.0.0") {
		t.Fatalf("release notes: %+v", r)
	}

	// --json stays the machine contract.
	r = runIn(t, "--json", "stack", "validate")
	var payload map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &payload); err != nil || payload["resources"] == nil {
		t.Fatalf("json validate: %v %+v", err, r)
	}

	// Outside a coordination repository the error names the fix.
	r = f.run(t, false, "release", "list")
	if r.code == cli.ExitOK || !strings.Contains(r.stderr, "not a stack") || !strings.Contains(r.stderr, "fxvcs stack init") {
		t.Fatalf("release list outside a stack: %+v", r)
	}
}
