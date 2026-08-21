package manifest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// Evaluate runs src as a FiveM manifest inside the sandbox and returns the
// recorded metadata. It never panics on hostile input; every failure is a
// typed error wrapping one of the package sentinels.
func Evaluate(ctx context.Context, src []byte, opts Options) (*Manifest, error) {
	opts = opts.withDefaults()
	if len(src) > opts.MaxSourceBytes {
		return nil, &ErrLimitExceeded{Kind: LimitSourceSize, Detail: fmt.Sprintf("%d bytes > %d", len(src), opts.MaxSourceBytes)}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	regSize := opts.MaxCallDepth * 32
	if regSize < 1024 {
		regSize = 1024
	}
	L := lua.NewState(lua.Options{
		SkipOpenLibs:        true,
		CallStackSize:       opts.MaxCallDepth,
		RegistrySize:        regSize,
		RegistryMaxSize:     0, // no growth: deep recursion fails as "registry overflow"
		IncludeGoStackTrace: false,
	})
	defer L.Close()

	ev := &evaluator{L: L, opts: opts, m: &Manifest{LegacyFormat: opts.Legacy}}
	ev.installLibraries()
	ev.installDirectives()

	budget := newBudgetContext(ctx, opts.MaxInstructions, opts.MaxAllocBytes, opts.WallClock)
	L.SetContext(budget)

	fn, err := L.Load(bytes.NewReader(src), opts.ChunkName)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSyntax, luaMessage(err))
	}
	L.Push(fn)
	runErr := L.PCall(0, 0, nil)

	// A tripped budget wins over whatever message the VM produced, and a
	// recorded sandbox failure wins over a script that "recovered" via pcall.
	if lim := budget.limitErr(); lim != nil {
		return nil, lim
	}
	if ev.failure != nil {
		return nil, ev.failure
	}
	if runErr != nil {
		msg := luaMessage(runErr)
		if strings.Contains(msg, "stack overflow") || strings.Contains(msg, "registry overflow") {
			return nil, &ErrLimitExceeded{Kind: LimitRecursion, Detail: msg}
		}
		return nil, fmt.Errorf("%w: %s", ErrRuntime, msg)
	}
	return ev.finish()
}

// luaMessage strips gopher-lua's stack-trace suffix from an error message so
// callers get a single deterministic line.
func luaMessage(err error) string {
	var apiErr *lua.ApiError
	if errors.As(err, &apiErr) {
		msg := apiErr.Object.String()
		if apiErr.Type == lua.ApiErrorSyntax && apiErr.Cause != nil {
			msg = apiErr.Cause.Error()
		}
		if i := strings.Index(msg, "\nstack traceback:"); i >= 0 {
			msg = msg[:i]
		}
		return strings.TrimSpace(msg)
	}
	msg := err.Error()
	if i := strings.Index(msg, "\nstack traceback:"); i >= 0 {
		msg = msg[:i]
	}
	return strings.TrimSpace(msg)
}

// ---------------------------------------------------------------------------
// evaluator
// ---------------------------------------------------------------------------

type evaluator struct {
	L    *lua.LState
	opts Options
	m    *Manifest

	entries int
	failure error
	ignored map[string]bool

	pendingDataFile string
	hasPendingData  bool
}

// fail records the first sandbox failure and raises a Lua error. The failure
// is sticky: even if the script catches the Lua error with pcall, Evaluate
// returns it.
func (e *evaluator) fail(err error) {
	if e.failure == nil {
		e.failure = err
	}
	e.L.RaiseError("%s", err.Error())
}

// removedBaseFunctions must never be reachable from a manifest.
var removedBaseFunctions = []string{
	"dofile", "load", "loadfile", "loadstring", "require", "module",
	"collectgarbage", "getfenv", "setfenv", "newproxy", "_printregs",
	"_GOPHER_LUA_VERSION",
}

// forbiddenGlobals produce ErrForbidden (rather than ErrUnknownDirective)
// when a manifest reads them, so diagnostics name the capability.
var forbiddenGlobals = map[string]bool{
	"os": true, "io": true, "package": true, "require": true, "debug": true,
	"coroutine": true, "load": true, "loadstring": true, "dofile": true,
	"loadfile": true, "collectgarbage": true, "getfenv": true, "setfenv": true,
	"module": true, "channel": true, "newproxy": true, "GetConvar": true,
}

func (e *evaluator) installLibraries() {
	L := e.L
	for _, lib := range []struct {
		name string
		fn   lua.LGFunction
	}{
		{lua.BaseLibName, lua.OpenBase},
		{lua.StringLibName, lua.OpenString},
		{lua.TabLibName, lua.OpenTable},
		{lua.MathLibName, lua.OpenMath},
	} {
		L.Push(L.NewFunction(lib.fn))
		L.Push(lua.LString(lib.name))
		L.Call(1, 0)
	}
	G := L.Get(lua.GlobalsIndex).(*lua.LTable)
	for _, name := range removedBaseFunctions {
		G.RawSetString(name, lua.LNil)
	}
	// print is a no-op: manifests must be pure, and stdout belongs to the CLI.
	G.RawSetString("print", L.NewFunction(func(L *lua.LState) int { return 0 }))

	if str, ok := G.RawGetString("string").(*lua.LTable); ok {
		str.RawSetString("dump", lua.LNil)
		str.RawSetString("rep", L.NewFunction(e.boundedRep))
	}
	if math, ok := G.RawGetString("math").(*lua.LTable); ok {
		math.RawSetString("random", lua.LNil)
		math.RawSetString("randomseed", lua.LNil)
	}

	// Unknown globals never evaluate to nil. Capability names fail closed;
	// anything else that looks like a metadata key becomes a recording
	// function, because FiveM lets a resource declare its own metadata and
	// read it back with GetResourceMetadata.
	mt := L.NewTable()
	mt.RawSetString("__index", L.NewFunction(func(L *lua.LState) int {
		key := L.Get(2)
		name := key.String()
		if forbiddenGlobals[name] {
			e.fail(fmt.Errorf("%w: %q is not available in the manifest sandbox", ErrForbidden, name))
			return 0
		}
		if !isMetadataKey(name) {
			e.fail(fmt.Errorf("%w: %q", ErrUnknownDirective, name))
			return 0
		}
		L.Push(L.NewFunction(func(L *lua.LState) int { return e.callMetadata(L, name) }))
		return 1
	}))
	L.SetMetatable(G, mt)
}

// boundedRep replaces string.rep so a single call cannot allocate past the
// string-length limit before the allocation budget notices.
func (e *evaluator) boundedRep(l *lua.LState) int {
	s := l.CheckString(1)
	n := l.CheckInt(2)
	if n < 0 {
		n = 0
	}
	if int64(len(s))*int64(n) > int64(e.opts.MaxStringLength) {
		e.fail(&ErrLimitExceeded{Kind: LimitStringLength, Detail: fmt.Sprintf("string.rep would produce %d bytes > %d", int64(len(s))*int64(n), e.opts.MaxStringLength)})
		return 0
	}
	l.Push(lua.LString(strings.Repeat(s, n)))
	return 1
}

// ---------------------------------------------------------------------------
// directives
// ---------------------------------------------------------------------------

type directiveKind int

const (
	kindScalar   directiveKind = iota // single string, last declaration wins
	kindList                          // ordered list, duplicates preserved
	kindSet                           // sorted unique set
	kindBool                          // yes/no flag
	kindDataFile                      // data_file 'TYPE' 'path'
	kindIgnored                       // tolerated cosmetic directive
)

type directive struct {
	kind directiveKind
	set  func(m *Manifest, v string)
	list func(m *Manifest) *[]string
	flag func(m *Manifest) *bool
}

// Directives is the closed allowlist of manifest functions. Anything else is
// ErrUnknownDirective.
var directives = map[string]directive{
	"fx_version":                {kind: kindScalar, set: func(m *Manifest, v string) { m.FxVersion = v }},
	"resource_manifest_version": {kind: kindScalar, set: func(m *Manifest, v string) { m.ResourceManifestVersion = v }},
	"name":                      {kind: kindScalar, set: func(m *Manifest, v string) { m.Name = v }},
	"author":                    {kind: kindScalar, set: func(m *Manifest, v string) { m.Author = v }},
	"description":               {kind: kindScalar, set: func(m *Manifest, v string) { m.Description = v }},
	"version":                   {kind: kindScalar, set: func(m *Manifest, v string) { m.Version = v }},
	"ui_page":                   {kind: kindScalar, set: func(m *Manifest, v string) { m.UIPage = v }},
	"loadscreen":                {kind: kindScalar, set: func(m *Manifest, v string) { m.Loadscreen = v }},

	"game":         {kind: kindSet, list: func(m *Manifest) *[]string { return &m.Games }},
	"games":        {kind: kindSet, list: func(m *Manifest) *[]string { return &m.Games }},
	"provide":      {kind: kindSet, list: func(m *Manifest) *[]string { return &m.Provides }},
	"provides":     {kind: kindSet, list: func(m *Manifest) *[]string { return &m.Provides }},
	"dependency":   {kind: kindSet, list: func(m *Manifest) *[]string { return &m.Dependencies }},
	"dependencies": {kind: kindSet, list: func(m *Manifest) *[]string { return &m.Dependencies }},

	"client_script":  {kind: kindList, list: func(m *Manifest) *[]string { return &m.ClientScripts }},
	"client_scripts": {kind: kindList, list: func(m *Manifest) *[]string { return &m.ClientScripts }},
	"server_script":  {kind: kindList, list: func(m *Manifest) *[]string { return &m.ServerScripts }},
	"server_scripts": {kind: kindList, list: func(m *Manifest) *[]string { return &m.ServerScripts }},
	"shared_script":  {kind: kindList, list: func(m *Manifest) *[]string { return &m.SharedScripts }},
	"shared_scripts": {kind: kindList, list: func(m *Manifest) *[]string { return &m.SharedScripts }},
	"file":           {kind: kindList, list: func(m *Manifest) *[]string { return &m.Files }},
	"files":          {kind: kindList, list: func(m *Manifest) *[]string { return &m.Files }},
	"export":         {kind: kindList, list: func(m *Manifest) *[]string { return &m.Exports }},
	"exports":        {kind: kindList, list: func(m *Manifest) *[]string { return &m.Exports }},
	"server_export":  {kind: kindList, list: func(m *Manifest) *[]string { return &m.ServerExports }},
	"server_exports": {kind: kindList, list: func(m *Manifest) *[]string { return &m.ServerExports }},

	"server_only":   {kind: kindBool, flag: func(m *Manifest) *bool { return &m.ServerOnly }},
	"lua54":         {kind: kindBool, flag: func(m *Manifest) *bool { return &m.Lua54 }},
	"this_is_a_map": {kind: kindBool, flag: func(m *Manifest) *bool { return &m.IsMap }},

	"data_file": {kind: kindDataFile},
}

// IgnoredDirectives are accepted and counted but not recorded. They are
// informational or affect only script-runtime behaviour, never activation,
// ordering, or which files exist. Keep this list short and explicit.
var IgnoredDirectives = []string{
	"url", "repository", "discord", "credits", "contact", "license",
	"escrow_ignore",
	"use_experimental_fxv2_oal", "use_fxv2_oal", "experimental_fxv2_oal",
	"clr_disable_task_scheduler", "disable_lazy_natives",
	"node_version", "mono_rt2", "rdr3_warning",
	"loadscreen_manual_shutdown", "loadscreen_cursor",
	"replace_level_meta", "before_level_meta", "after_level_meta",
	"map", "convar_category", "data_environment", "is_cfxv2",
	"client_export", "client_exports", // deprecated aliases with no activation effect
}

// RecordedDirectives returns the sorted allowlist of recorded directive names.
func RecordedDirectives() []string {
	out := make([]string, 0, len(directives))
	for k := range directives {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (e *evaluator) installDirectives() {
	L := e.L
	G := L.Get(lua.GlobalsIndex).(*lua.LTable)
	for name, d := range directives {
		name, d := name, d
		G.RawSetString(name, L.NewFunction(func(L *lua.LState) int { return e.callDirective(L, name, d) }))
	}
	for _, name := range IgnoredDirectives {
		name := name
		G.RawSetString(name, L.NewFunction(func(L *lua.LState) int { return e.callDirective(L, name, directive{kind: kindIgnored}) }))
	}
}

// chainedFn returns the function value a directive call evaluates to, so the
// FiveM chaining style `data_file 'TYPE' 'path'` works. For every directive
// other than data_file a chained value is an error: FiveM would record it
// under an unrelated "<key>_extra" metadata key, which is never intended.
func (e *evaluator) chainedFn(name string, d directive) *lua.LFunction {
	return e.L.NewFunction(func(L *lua.LState) int {
		if d.kind == kindDataFile {
			if !e.hasPendingData {
				e.fail(fmt.Errorf("%w: data_file path without type", ErrBadArgument))
				return 0
			}
			path, err := e.scalar(L.Get(1))
			if err != nil {
				e.fail(argError("data_file path", err))
				return 0
			}
			if !e.reserve(1) {
				return 0
			}
			e.m.DataFiles = append(e.m.DataFiles, DataFile{Type: e.pendingDataFile, Path: path})
			e.hasPendingData = false
			e.pendingDataFile = ""
			L.Push(e.ignoreChain())
			return 1
		}
		e.fail(fmt.Errorf("%w: chained value after %s is not supported", ErrBadArgument, name))
		return 0
	})
}

// ignoreChain returns a self-returning function used where further chained
// values carry no meaning.
func (e *evaluator) ignoreChain() *lua.LFunction {
	var fn *lua.LFunction
	fn = e.L.NewFunction(func(L *lua.LState) int {
		L.Push(fn)
		return 1
	})
	return fn
}

// isMetadataKey reports whether name has the shape of a FiveM metadata key.
// Only plain Lua identifiers qualify, so a dynamically built name
// (`_G['not a key']`) still fails closed as an unknown directive.
func isMetadataKey(name string) bool {
	if name == "" || len(name) > maxMetadataKeyLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// maxMetadataKeyLength bounds a custom metadata key so a generated manifest
// cannot grow the Metadata map with arbitrarily long names.
const maxMetadataKeyLength = 128

// callMetadata records a resource-defined metadata entry. FiveM stores every
// unrecognised manifest call as metadata under its name and exposes it to
// scripts through GetResourceMetadata, so these are declarations, not errors.
func (e *evaluator) callMetadata(l *lua.LState, name string) int {
	if e.hasPendingData {
		e.fail(fmt.Errorf("%w: data_file %q is missing its path", ErrInvalid, e.pendingDataFile))
		return 0
	}
	values, err := e.values(l.Get(1))
	if err != nil {
		e.fail(argError(name, err))
		return 0
	}
	if !e.reserve(len(values)) {
		return 0
	}
	if e.m.Metadata == nil {
		e.m.Metadata = map[string][]string{}
	}
	if _, seen := e.m.Metadata[name]; !seen && len(e.m.Metadata) >= e.opts.MaxEntries {
		e.fail(&ErrLimitExceeded{Kind: LimitOutput, Detail: fmt.Sprintf("more than %d metadata keys", e.opts.MaxEntries)})
		return 0
	}
	e.m.Directives++
	e.m.Metadata[name] = append(e.m.Metadata[name], values...)
	// Chaining carries no meaning for custom metadata, and FiveM would file a
	// chained value under an unrelated `<key>_extra` key.
	l.Push(e.L.NewFunction(func(l *lua.LState) int {
		e.fail(fmt.Errorf("%w: chained value after %s is not supported", ErrBadArgument, name))
		return 0
	}))
	return 1
}

func (e *evaluator) callDirective(l *lua.LState, name string, d directive) int {
	if e.hasPendingData {
		e.fail(fmt.Errorf("%w: data_file %q is missing its path", ErrInvalid, e.pendingDataFile))
		return 0
	}
	e.m.Directives++
	arg := l.Get(1)
	switch d.kind {
	case kindIgnored:
		if e.ignored == nil {
			e.ignored = map[string]bool{}
		}
		e.ignored[name] = true
		// cosmetic directives may chain (e.g. rare `url 'a' 'b'`); ignore.
		l.Push(e.ignoreChain())
		return 1
	case kindDataFile:
		typ, err := e.scalar(arg)
		if err != nil {
			e.fail(argError(name, err))
			return 0
		}
		e.pendingDataFile = typ
		e.hasPendingData = true
		l.Push(e.chainedFn(name, d))
		return 1
	}
	values, err := e.values(arg)
	if err != nil {
		e.fail(argError(name, err))
		return 0
	}
	switch d.kind {
	case kindScalar:
		if len(values) != 1 {
			e.fail(fmt.Errorf("%w: %s expects exactly one string", ErrBadArgument, name))
			return 0
		}
		d.set(e.m, values[0])
	case kindBool:
		if len(values) != 1 {
			e.fail(fmt.Errorf("%w: %s expects exactly one value", ErrBadArgument, name))
			return 0
		}
		b, ok := parseFlag(values[0])
		if !ok {
			e.fail(fmt.Errorf("%w: %s: %q is not a yes/no value", ErrBadArgument, name, values[0]))
			return 0
		}
		*d.flag(e.m) = b
	case kindList, kindSet:
		if !e.reserve(len(values)) {
			return 0
		}
		lst := d.list(e.m)
		*lst = append(*lst, values...)
	}
	l.Push(e.chainedFn(name, d))
	return 1
}

// argError classifies a value-conversion failure: limit errors keep their
// type, everything else is ErrBadArgument.
func argError(name string, err error) error {
	var lim *ErrLimitExceeded
	if errors.As(err, &lim) {
		return lim
	}
	return fmt.Errorf("%w: %s: %v", ErrBadArgument, name, err)
}

// reserve accounts n recorded entries against the output cap.
func (e *evaluator) reserve(n int) bool {
	e.entries += n
	if e.entries > e.opts.MaxEntries {
		e.fail(&ErrLimitExceeded{Kind: LimitOutput, Detail: fmt.Sprintf("more than %d recorded entries", e.opts.MaxEntries)})
		return false
	}
	return true
}

func parseFlag(v string) (bool, bool) {
	switch strings.ToLower(v) {
	case "yes", "true", "1", "on":
		return true, true
	case "no", "false", "0", "off":
		return false, true
	}
	return false, false
}

// scalar converts a Lua string/number/boolean to its recorded string.
func (e *evaluator) scalar(v lua.LValue) (string, error) {
	var s string
	switch x := v.(type) {
	case lua.LString:
		s = string(x)
	case lua.LNumber:
		s = x.String()
	case lua.LBool:
		if x {
			s = "true"
		} else {
			s = "false"
		}
	case *lua.LNilType:
		return "", errors.New("value is nil")
	default:
		return "", fmt.Errorf("unsupported value type %s", v.Type().String())
	}
	if len(s) > e.opts.MaxStringLength {
		return "", &ErrLimitExceeded{Kind: LimitStringLength, Detail: fmt.Sprintf("%d bytes > %d", len(s), e.opts.MaxStringLength)}
	}
	if strings.ContainsRune(s, 0) {
		return "", errors.New("value contains NUL")
	}
	return s, nil
}

// values accepts a scalar or a sequence table of scalars.
func (e *evaluator) values(v lua.LValue) ([]string, error) {
	t, ok := v.(*lua.LTable)
	if !ok {
		s, err := e.scalar(v)
		if err != nil {
			return nil, err
		}
		return []string{s}, nil
	}
	n := t.Len()
	if n > e.opts.MaxEntries {
		return nil, &ErrLimitExceeded{Kind: LimitOutput, Detail: fmt.Sprintf("table with %d entries > %d", n, e.opts.MaxEntries)}
	}
	total := 0
	var iterErr error
	t.ForEach(func(k, _ lua.LValue) {
		total++
		if iterErr != nil {
			return
		}
		kn, isNum := k.(lua.LNumber)
		if !isNum || float64(int(kn)) != float64(kn) || int(kn) < 1 || int(kn) > n {
			iterErr = fmt.Errorf("table must be a plain sequence of strings (unexpected key %s)", k.String())
		}
	})
	if iterErr != nil {
		return nil, iterErr
	}
	if total != n {
		return nil, errors.New("table must be a plain sequence of strings (holes are not allowed)")
	}
	out := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		s, err := e.scalar(t.RawGetInt(i))
		if err != nil {
			var lim *ErrLimitExceeded
			if errors.As(err, &lim) {
				return nil, lim
			}
			return nil, fmt.Errorf("entry %d: %v", i, err)
		}
		out = append(out, s)
	}
	return out, nil
}

// finish validates the recorded manifest and normalizes ordering.
func (e *evaluator) finish() (*Manifest, error) {
	m := e.m
	if e.hasPendingData {
		return nil, fmt.Errorf("%w: data_file %q is missing its path", ErrInvalid, e.pendingDataFile)
	}
	if !m.LegacyFormat {
		if m.FxVersion == "" {
			return nil, fmt.Errorf("%w: fxmanifest.lua must declare fx_version", ErrInvalid)
		}
		if len(m.Games) == 0 {
			return nil, fmt.Errorf("%w: fxmanifest.lua must declare game or games", ErrInvalid)
		}
	} else if m.ResourceManifestVersion == "" && m.FxVersion == "" {
		return nil, fmt.Errorf("%w: __resource.lua must declare resource_manifest_version", ErrInvalid)
	}
	m.Games = sortedUnique(m.Games)
	m.Provides = sortedUnique(m.Provides)
	// Split runtime capability requirements ("/server:4500", "/onesync", ...)
	// out of the resource dependency set.
	var deps []string
	for _, d := range m.Dependencies {
		if strings.HasPrefix(d, "/") {
			m.RuntimeRequirements = append(m.RuntimeRequirements, d)
		} else {
			deps = append(deps, d)
		}
	}
	m.Dependencies = sortedUnique(deps)
	m.RuntimeRequirements = sortedUnique(m.RuntimeRequirements)
	if m.Name == "" {
		m.Name = e.opts.DefaultName
	}
	m.Ignored = make([]string, 0, len(e.ignored))
	for k := range e.ignored {
		m.Ignored = append(m.Ignored, k)
	}
	sort.Strings(m.Ignored)
	// Never emit null lists.
	for _, p := range []*[]string{&m.Games, &m.Provides, &m.Dependencies, &m.RuntimeRequirements, &m.ClientScripts, &m.ServerScripts, &m.SharedScripts, &m.Files, &m.Exports, &m.ServerExports} {
		if *p == nil {
			*p = []string{}
		}
	}
	if m.DataFiles == nil {
		m.DataFiles = []DataFile{}
	}
	if m.Metadata == nil {
		m.Metadata = map[string][]string{}
	}
	return m, nil
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	j := 0
	for i := range out {
		if i == 0 || out[i] != out[i-1] {
			out[j] = out[i]
			j++
		}
	}
	return out[:j]
}
