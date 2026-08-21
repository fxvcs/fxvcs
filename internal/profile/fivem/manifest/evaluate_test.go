package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

const fixtureRoot = "../../../../spec/fixtures/fivem"

func eval(t *testing.T, src string, opts Options) (*Manifest, error) {
	t.Helper()
	return Evaluate(context.Background(), []byte(src), opts)
}

func TestEvaluateFullManifest(t *testing.T) {
	src := `
fx_version 'cerulean'
games { 'gta5', 'rdr3', 'gta5' }
lua54 'yes'
name 'Inventory'
author 'community'
description 'items'
version '1.2.3'
client_script 'client/main.lua'
client_scripts { 'client/a.lua', 'client/b.lua' }
server_scripts { '@oxmysql/lib/MySQL.lua', 'server/main.lua' }
shared_script 'config.lua'
files { 'html/index.html', 'html/app.js' }
ui_page 'html/index.html'
provide 'mapmanager'
dependency 'es_extended'
dependencies { 'oxmysql', 'es_extended', '/server:4500', '/onesync', '/gameBuild:h4', '/policy:subdir_file_mapping', '/native:0xE27C97A0' }
server_only 'no'
data_file 'DLC_ITYP_REQUEST' 'stream/props.ytyp'
exports { 'GetItem', 'AddItem' }
server_export 'RemoveItem'
this_is_a_map 'yes'
loadscreen 'html/load.html'
url 'https://example.invalid'
escrow_ignore { 'config.lua' }
`
	m, err := eval(t, src, Options{DefaultName: "inventory"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	want := &Manifest{
		FxVersion:           "cerulean",
		Games:               []string{"gta5", "rdr3"},
		Name:                "Inventory",
		Author:              "community",
		Description:         "items",
		Version:             "1.2.3",
		Provides:            []string{"mapmanager"},
		Dependencies:        []string{"es_extended", "oxmysql"},
		RuntimeRequirements: []string{"/gameBuild:h4", "/native:0xE27C97A0", "/onesync", "/policy:subdir_file_mapping", "/server:4500"},
		Lua54:               true,
		ClientScripts:       []string{"client/main.lua", "client/a.lua", "client/b.lua"},
		ServerScripts:       []string{"@oxmysql/lib/MySQL.lua", "server/main.lua"},
		SharedScripts:       []string{"config.lua"},
		Files:               []string{"html/index.html", "html/app.js"},
		UIPage:              "html/index.html",
		Loadscreen:          "html/load.html",
		Exports:             []string{"GetItem", "AddItem"},
		ServerExports:       []string{"RemoveItem"},
		DataFiles:           []DataFile{{Type: "DLC_ITYP_REQUEST", Path: "stream/props.ytyp"}},
		IsMap:               true,
		Directives:          24,
		Ignored:             []string{"escrow_ignore", "url"},
		Metadata:            map[string][]string{},
	}
	if diff := cmp.Diff(want, m); diff != "" {
		t.Fatalf("manifest mismatch (-want +got):\n%s", diff)
	}
}

func TestEvaluateLegacyAndDefaults(t *testing.T) {
	m, err := eval(t, "resource_manifest_version '44febabe-d386-4d18-afbe-5e627f4af937'\nclient_script 'a.lua'", Options{Legacy: true, DefaultName: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.LegacyFormat || m.Name != "chat" || m.ResourceManifestVersion == "" {
		t.Fatalf("unexpected: %+v", m)
	}
	// Legacy manifests are not required to declare game.
	if len(m.Games) != 0 {
		t.Fatalf("games: %v", m.Games)
	}
}

func TestEvaluateCustomMetadata(t *testing.T) {
	// Real-world shape: a resource declares its own metadata key and reads it
	// back at runtime with GetResourceMetadata.
	src := `
resource_manifest_version '44febabe-d386-4d18-afbe-5e627f4af937'
local uniformFile = 'outfits.json'
file(uniformFile)
uniform_file(uniformFile)
client_script 'eup_ui.lua'
my_list { 'a', 'b' }
my_list 'c'
`
	m, err := eval(t, src, Options{Legacy: true, DefaultName: "ELRP-PubcopMenu"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	want := map[string][]string{
		"uniform_file": {"outfits.json"},
		"my_list":      {"a", "b", "c"},
	}
	if diff := cmp.Diff(want, m.Metadata); diff != "" {
		t.Fatalf("metadata mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"outfits.json"}, m.Files); diff != "" {
		t.Fatalf("files mismatch (-want +got):\n%s", diff)
	}
	// Custom metadata counts as a directive call, like every other one.
	if m.Directives != 6 {
		t.Fatalf("directives: %d", m.Directives)
	}
}

func TestEvaluateCustomMetadataIsBounded(t *testing.T) {
	src := "fx_version 'cerulean'\ngame 'gta5'\nfor i = 1, 100 do my_key 'x' end"
	_, err := eval(t, src, Options{MaxEntries: 10})
	var lim *ErrLimitExceeded
	if !errors.As(err, &lim) || lim.Kind != LimitOutput {
		t.Fatalf("got %v, want output limit", err)
	}
}

func TestEvaluateIsDeterministic(t *testing.T) {
	src := "fx_version 'cerulean'\ngame 'gta5'\nlocal t = {}\nfor i = 1, 20 do t[#t+1] = 'f' .. i .. '.lua' end\nfiles(t)\nprovides { 'z', 'a', 'm' }"
	a, err := eval(t, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b, err := eval(t, src, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(a, b); diff != "" {
			t.Fatalf("run %d differs:\n%s", i, diff)
		}
	}
	if a.Files[0] != "f1.lua" || a.Files[19] != "f20.lua" || strings.Join(a.Provides, ",") != "a,m,z" {
		t.Fatalf("ordering: %+v", a)
	}
}

func TestEvaluatePureLuaIsAllowed(t *testing.T) {
	src := `
fx_version 'cerulean'
game 'gta5'
local scripts = {}
for _, n in ipairs({ 'a', 'b' }) do table.insert(scripts, string.format('%s.lua', n)) end
table.sort(scripts)
client_scripts(scripts)
version(tostring(math.floor(1.9)))
if #scripts == 2 then server_only 'yes' end
print('ignored')
`
	m, err := eval(t, src, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !m.ServerOnly || m.Version != "1" || len(m.ClientScripts) != 2 {
		t.Fatalf("%+v", m)
	}
}

func TestEvaluateFailures(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want error
	}{
		{"syntax", "fx_version 'cerulean", ErrSyntax},
		{"non-identifier global", "fx_version 'cerulean'\ngame 'gta5'\nlocal x = _G['not a key']", ErrUnknownDirective},
		{"chained metadata", "fx_version 'cerulean'\ngame 'gta5'\nmy_custom_key 'a' 'b'", ErrBadArgument},
		{"missing fx_version", "game 'gta5'", ErrInvalid},
		{"missing game", "fx_version 'cerulean'", ErrInvalid},
		{"legacy without version", "client_script 'a.lua'", ErrInvalid},
		{"bad bool", "fx_version 'cerulean'\ngame 'gta5'\nserver_only 'maybe'", ErrBadArgument},
		{"nested table", "fx_version 'cerulean'\ngame 'gta5'\nfiles { { 'a' } }", ErrBadArgument},
		{"map table", "fx_version 'cerulean'\ngame 'gta5'\nfiles { a = 'b' }", ErrBadArgument},
		{"chained script", "fx_version 'cerulean'\ngame 'gta5'\nclient_script 'a.lua' 'b.lua'", ErrBadArgument},
		{"data_file without path", "fx_version 'cerulean'\ngame 'gta5'\ndata_file 'DLC_ITYP_REQUEST'", ErrInvalid},
		{"runtime error", "fx_version 'cerulean'\ngame 'gta5'\nerror('boom')", ErrRuntime},
		{"nil arg", "fx_version 'cerulean'\ngame 'gta5'\nclient_script(nil)", ErrBadArgument},
		{"pcall cannot hide forbidden", "fx_version 'cerulean'\ngame 'gta5'\npcall(function() return io.open('x') end)", ErrForbidden},
		{"pcall cannot hide non-identifier", "fx_version 'cerulean'\ngame 'gta5'\npcall(function() return _G['no good'] end)", ErrUnknownDirective},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{}
			if tc.name == "legacy without version" {
				opts.Legacy = true
			}
			_, err := eval(t, tc.src, opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestForbiddenCapabilities(t *testing.T) {
	head := "fx_version 'cerulean'\ngame 'gta5'\n"
	cases := map[string]string{
		"io":             "io.open('/etc/passwd')",
		"os":             "os.getenv('HOME')",
		"require":        "require('socket')",
		"load":           "load('return 1')()",
		"loadstring":     "loadstring('return 1')()",
		"dofile":         "dofile('x.lua')",
		"loadfile":       "loadfile('x.lua')",
		"package":        "package.path = ''",
		"debug":          "debug.getinfo(1)",
		"coroutine":      "coroutine.create(function() end)",
		"collectgarbage": "collectgarbage('count')",
		"setfenv":        "setfenv(1, {})",
		"channel":        "channel.make()",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := eval(t, head+body, Options{})
			if !errors.Is(err, ErrForbidden) {
				t.Fatalf("%s: got %v, want ErrForbidden", name, err)
			}
		})
	}
	t.Run("string.dump", func(t *testing.T) {
		_, err := eval(t, head+"string.dump(function() end)", Options{})
		if !errors.Is(err, ErrRuntime) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("math.random", func(t *testing.T) {
		_, err := eval(t, head+"local x = math.random()", Options{})
		if !errors.Is(err, ErrRuntime) {
			t.Fatalf("got %v", err)
		}
	})
}

// smallLimits keeps every limit test in the millisecond range.
var smallLimits = Options{
	MaxInstructions: 20_000,
	MaxAllocBytes:   1 << 20,
	MaxCallDepth:    64,
	MaxEntries:      100,
	MaxStringLength: 512,
	MaxSourceBytes:  64 << 10,
	WallClock:       5 * time.Second,
}

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("fixture %s: %v", rel, err)
	}
	return b
}

func TestLimitFixtures(t *testing.T) {
	cases := []struct {
		file string
		kind LimitKind
		want error
	}{
		{"limits/infinite-loop.lua", LimitInstructions, ErrLimit},
		{"limits/deep-recursion.lua", LimitRecursion, ErrLimit},
		{"limits/memory-bomb.lua", LimitMemory, ErrLimit},
		{"limits/huge-table.lua", LimitOutput, ErrLimit},
		{"limits/string-rep.lua", LimitStringLength, ErrLimit},
		{"limits/forbidden-io.lua", "", ErrForbidden},
		{"limits/forbidden-require.lua", "", ErrForbidden},
		{"limits/forbidden-os.lua", "", ErrForbidden},
		{"limits/dynamic-load.lua", "", ErrForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			start := time.Now()
			opts := smallLimits
			if tc.kind == LimitOutput {
				// Let the loop that builds the table run; the output cap must be
				// what trips (also under the default limits: 20000 entries > 10000).
				opts.MaxInstructions = 500_000
				opts.MaxAllocBytes = 32 << 20
			}
			_, err := Evaluate(context.Background(), readFixture(t, tc.file), opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if tc.kind != "" {
				var lim *ErrLimitExceeded
				if !errors.As(err, &lim) || lim.Kind != tc.kind {
					t.Fatalf("got %v, want limit kind %s", err, tc.kind)
				}
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("limit took too long: %s", d)
			}
		})
	}
}

func TestWallClockAndCancellation(t *testing.T) {
	src := "fx_version 'cerulean'\ngame 'gta5'\nwhile true do end"
	opts := smallLimits
	opts.MaxInstructions = 1 << 62
	opts.WallClock = 30 * time.Millisecond
	_, err := Evaluate(context.Background(), []byte(src), opts)
	var lim *ErrLimitExceeded
	if !errors.As(err, &lim) || lim.Kind != LimitWallClock {
		t.Fatalf("got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	opts.WallClock = time.Minute
	_, err = Evaluate(ctx, []byte(src), opts)
	if !errors.As(err, &lim) || lim.Kind != LimitWallClock {
		t.Fatalf("cancelled ctx: got %v", err)
	}
}

func TestSourceSizeLimit(t *testing.T) {
	opts := smallLimits
	opts.MaxSourceBytes = 16
	_, err := eval(t, "fx_version 'cerulean'\ngame 'gta5'", opts)
	var lim *ErrLimitExceeded
	if !errors.As(err, &lim) || lim.Kind != LimitSourceSize {
		t.Fatalf("got %v", err)
	}
}

func TestBudgetIsNotCatchableByPcall(t *testing.T) {
	src := "fx_version 'cerulean'\ngame 'gta5'\nwhile true do pcall(function() while true do end end) end"
	_, err := eval(t, src, smallLimits)
	var lim *ErrLimitExceeded
	if !errors.As(err, &lim) || lim.Kind != LimitInstructions {
		t.Fatalf("got %v", err)
	}
}
