package app

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"fxvcs.dev/fxvcs/internal/profile/fivem/discovery"
)

// Server configuration import (`fxvcs resources sync --from-cfg <server.cfg>`).
//
// The parser is a deliberately small, deterministic reader of FXServer console
// configuration files. It understands exactly what activation seeding needs:
//
//   - `ensure <name...>` and `start <name...>` name resources that the server
//     starts at boot; every valid name joins the result set;
//   - `stop <name...>` is recorded as a warning and otherwise ignored: a cfg
//     that stops a resource proves nothing about the operator's intent, so the
//     importer never guesses `disabled`;
//   - `exec <file>` is followed recursively. The file is resolved relative to
//     the directory of the cfg that names it and must stay beneath the root
//     cfg's directory: absolute paths, traversal outside that root, exec depth
//     beyond CfgMaxExecDepth, and exec cycles are errors. `@resource/file.cfg`
//     (resource-relative) cannot be resolved without a running server and is
//     skipped with a warning. A file that was already fully parsed is not
//     parsed again;
//   - `#` starts a comment; single and double quotes group a token; every
//     other directive (`set`, `sets`, `setr`, `add_ace`, `sv_*`, …) is ignored.
//
// It never touches anything outside the cfg root and never executes anything.

// CfgMaxExecDepth bounds nested `exec` chains.
const CfgMaxExecDepth = 16

// ErrCfg classifies every server.cfg import failure.
var ErrCfg = errors.New("app: server.cfg import")

// CfgError is a typed server.cfg import failure. File is the offending cfg
// path relative to the cfg root ("" for the root file itself when it cannot be
// read); Line is 1-based (0 when not line-specific).
type CfgError struct {
	File   string
	Line   int
	Reason string // "unreadable" | "absolute-path" | "escapes-root" | "cycle" | "too-deep"
	Detail string
	Err    error
}

func (e *CfgError) Error() string {
	var b strings.Builder
	b.WriteString("app: server.cfg import: ")
	b.WriteString(e.Reason)
	if e.File != "" {
		fmt.Fprintf(&b, " at %s", e.File)
		if e.Line > 0 {
			fmt.Fprintf(&b, ":%d", e.Line)
		}
	}
	if e.Detail != "" {
		b.WriteString(": ")
		b.WriteString(e.Detail)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *CfgError) Unwrap() error { return e.Err }

func (e *CfgError) Is(target error) bool { return target == ErrCfg }

// CfgImport is the result of parsing a server configuration tree.
type CfgImport struct {
	// Root is the absolute directory of the top-level cfg; every File is
	// relative to it.
	Root string `json:"root"`
	// Resources are the resource names started by ensure/start, sorted and
	// de-duplicated.
	Resources []string `json:"resources"`
	// Files lists every cfg file that was parsed, root-relative with '/'
	// separators, in first-visit order.
	Files []string `json:"files"`
	// Warnings describe skipped constructs (resource-relative exec, stop
	// lines, invalid names) as "<file>:<line>: <message>".
	Warnings []string `json:"warnings"`
}

// Has reports whether name was started by the imported configuration.
func (c CfgImport) Has(name string) bool {
	i := sort.SearchStrings(c.Resources, name)
	return i < len(c.Resources) && c.Resources[i] == name
}

// ParseServerCfg parses path and every cfg it execs (see the package notes
// above). The cfg root is the directory containing path.
func ParseServerCfg(path string) (CfgImport, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return CfgImport{}, &CfgError{Reason: "unreadable", Detail: path, Err: err}
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	p := &cfgParser{
		root: filepath.Dir(abs),
		out:  CfgImport{Root: filepath.Dir(abs), Resources: []string{}, Files: []string{}, Warnings: []string{}},
		seen: map[string]bool{},
		done: map[string]bool{},
	}
	rel := filepath.Base(abs)
	if err := p.parseFile(abs, rel, 0); err != nil {
		return CfgImport{}, err
	}
	names := make([]string, 0, len(p.seen))
	for n := range p.seen {
		names = append(names, n)
	}
	sort.Strings(names)
	p.out.Resources = names
	return p.out, nil
}

type cfgParser struct {
	root  string
	out   CfgImport
	seen  map[string]bool // resource names
	done  map[string]bool // fully parsed files (absolute)
	stack []string        // files being parsed (absolute)
}

func (p *cfgParser) warn(file string, line int, msg string) {
	p.out.Warnings = append(p.out.Warnings, fmt.Sprintf("%s:%d: %s", file, line, msg))
}

func (p *cfgParser) parseFile(abs, rel string, depth int) error {
	if depth > CfgMaxExecDepth {
		return &CfgError{File: rel, Reason: "too-deep", Detail: fmt.Sprintf("exec nesting exceeds %d", CfgMaxExecDepth)}
	}
	for _, s := range p.stack {
		if s == abs {
			return &CfgError{File: rel, Reason: "cycle", Detail: "exec chain: " + p.chain(abs)}
		}
	}
	if p.done[abs] {
		return nil
	}
	f, err := os.Open(abs) //nolint:gosec // resolved beneath the cfg root by resolveExec
	if err != nil {
		return &CfgError{File: rel, Reason: "unreadable", Err: err}
	}
	defer func() { _ = f.Close() }()
	p.stack = append(p.stack, abs)
	defer func() { p.stack = p.stack[:len(p.stack)-1] }()
	p.out.Files = append(p.out.Files, rel)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		toks := cfgTokens(sc.Text())
		if len(toks) == 0 {
			continue
		}
		switch strings.ToLower(toks[0]) {
		case "ensure", "start":
			for _, name := range toks[1:] {
				if err := discovery.ValidateResourceName(name); err != nil {
					p.warn(rel, line, fmt.Sprintf("ignored %s %q: %v", toks[0], name, err))
					continue
				}
				p.seen[name] = true
			}
		case "stop":
			p.warn(rel, line, fmt.Sprintf("ignored stop %s (cfg import never infers disabled)", strings.Join(toks[1:], " ")))
		case "exec":
			if len(toks) < 2 {
				p.warn(rel, line, "ignored exec without a file")
				continue
			}
			target := toks[1]
			if strings.HasPrefix(target, "@") {
				p.warn(rel, line, fmt.Sprintf("skipped resource-relative exec %q (cannot be resolved without a running server)", target))
				continue
			}
			nextAbs, nextRel, err := p.resolveExec(filepath.Dir(abs), target, rel, line)
			if err != nil {
				return err
			}
			if err := p.parseFile(nextAbs, nextRel, depth+1); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return &CfgError{File: rel, Reason: "unreadable", Err: err}
	}
	p.done[abs] = true
	return nil
}

// resolveExec resolves target relative to dir and enforces the root sandbox.
func (p *cfgParser) resolveExec(dir, target, rel string, line int) (abs, nextRel string, err error) {
	t := filepath.FromSlash(target)
	if filepath.IsAbs(t) || filepath.VolumeName(t) != "" || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "\\") {
		return "", "", &CfgError{File: rel, Line: line, Reason: "absolute-path", Detail: fmt.Sprintf("exec %q", target)}
	}
	abs = filepath.Clean(filepath.Join(dir, t))
	r, relErr := filepath.Rel(p.root, abs)
	if relErr != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return "", "", &CfgError{File: rel, Line: line, Reason: "escapes-root", Detail: fmt.Sprintf("exec %q resolves outside %s", target, p.root)}
	}
	// A symlink beneath the root that points outside is refused too.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		if rr, err := filepath.Rel(p.root, resolved); err != nil || rr == ".." || strings.HasPrefix(rr, ".."+string(filepath.Separator)) {
			return "", "", &CfgError{File: rel, Line: line, Reason: "escapes-root", Detail: fmt.Sprintf("exec %q resolves outside %s", target, p.root)}
		}
		abs = resolved
	}
	return abs, filepath.ToSlash(r), nil
}

func (p *cfgParser) chain(abs string) string {
	parts := make([]string, 0, len(p.stack)+1)
	for _, s := range append(append([]string{}, p.stack...), abs) {
		if r, err := filepath.Rel(p.root, s); err == nil {
			parts = append(parts, filepath.ToSlash(r))
		} else {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, " -> ")
}

// cfgTokens splits one cfg line into tokens: whitespace-separated, quotes
// group, `#` outside quotes starts a comment.
func cfgTokens(line string) []string {
	var toks []string
	var cur strings.Builder
	inTok := false
	var quote byte
	flush := func() {
		if inTok {
			toks = append(toks, cur.String())
			cur.Reset()
			inTok = false
		}
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case quote != 0:
			if ch == quote {
				quote = 0
			} else {
				cur.WriteByte(ch)
			}
		case ch == '"' || ch == '\'':
			quote = ch
			inTok = true
		case ch == '#':
			flush()
			return toks
		case ch == ' ' || ch == '\t' || ch == '\r' || ch == '\n':
			flush()
		default:
			cur.WriteByte(ch)
			inTok = true
		}
	}
	flush()
	return toks
}
