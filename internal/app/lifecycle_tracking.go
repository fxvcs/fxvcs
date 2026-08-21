package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"fxvcs.dev/fxvcs/internal/repository"
	"gopkg.in/yaml.v3"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"fxvcs.dev/fxvcs/internal/journal"
	"fxvcs.dev/fxvcs/internal/pathsafe"
	"fxvcs.dev/fxvcs/internal/pointer"
	"fxvcs.dev/fxvcs/internal/profile/fivem"
)

// AttributesPath is the root .gitattributes file that carries the managed
// block. Nested .gitattributes files are honored by Git but never edited by
// FXVCS.
const AttributesPath = ".gitattributes"

// Rule sources reported by TrackingCheck.
const (
	RuleSourceBlock = "fxvcs-block" // a line inside the managed block
	RuleSourceUser  = "user"        // a line of .gitattributes outside the block
	RuleSourceOther = "other"       // effective attribute comes from elsewhere (nested file, info/attributes, global)
	RuleSourceNone  = "none"
)

// ErrRuleCovered: untrack of a literal path that has no exact rule but is
// still matched by another rule.
var ErrRuleCovered = errors.New("app: path is covered by another tracking rule")

// sourceExtensions are text formats a broad glob must not capture without
// Force (docs/product-scope.md "Track content").
var sourceExtensions = map[string]bool{
	".lua": true, ".js": true, ".ts": true, ".json": true, ".cfg": true, ".meta": true,
	".xml": true, ".html": true, ".css": true, ".txt": true, ".md": true, ".yaml": true, ".yml": true,
}

// TrackInput selects rules to add or remove.
type TrackInput struct {
	// Patterns are literal repository-relative paths, or gitattributes glob
	// patterns when Glob is true.
	Patterns []string
	Glob     bool
	// Force allows a broad glob that would capture known source formats.
	Force bool
}

// TrackResult reports the rules added to the managed block.
type TrackResult struct {
	RulesAdded   []string
	RulesPresent []string
	Changed      bool
}

// UntrackResult reports rules removed from the managed block.
type UntrackResult struct {
	RulesRemoved []string
	RulesMissing []string
	// PathsRequiringHydration are index entries that were pointers under a
	// removed rule and match no remaining rule; they must be hydrated before
	// they can be staged as ordinary blobs. Nothing is hydrated here.
	PathsRequiringHydration []string
	Changed                 bool
}

// TrackingCheck explains the effective rule for one path.
type TrackingCheck struct {
	File          string
	Tracked       bool
	MatchedRule   string
	RuleSource    string
	AttributesRaw string
}

// ScanCandidate is one large working-tree file.
type ScanCandidate struct {
	Path    string
	Bytes   int64
	Tracked bool
}

// ---------------------------------------------------------------------------
// rules
// ---------------------------------------------------------------------------

// LiteralRule converts a repository-relative path into the exact
// .gitattributes pattern that matches it and nothing else: the path is
// pathsafe-normalized, anchored with a leading slash (an unanchored pattern
// without a slash would match the basename anywhere), glob metacharacters
// (\ * ? [ ]) are backslash-escaped so FiveM bracket directories match
// literally, an existing directory gets a trailing "/**" (attributes never
// apply to a directory recursively), and when the result contains
// whitespace it is C-escaped because AttributesBlock wraps such patterns in
// double quotes that Git unquotes C-style.
func LiteralRule(root, rel string) (string, error) {
	n, err := pathsafe.Normalize(rel)
	if err != nil {
		return "", err
	}
	if n == "." {
		return "", fmt.Errorf("%w: cannot track the repository root", ErrInvalidArgument)
	}
	var b strings.Builder
	b.WriteByte('/')
	for _, ch := range n {
		switch ch {
		case '\\', '*', '?', '[', ']':
			b.WriteByte('\\')
		}
		b.WriteRune(ch)
	}
	rule := b.String()
	if root != "" {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(n))); err == nil && fi.IsDir() {
			rule += "/**"
		}
	}
	if strings.ContainsAny(rule, " \t") {
		rule = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(rule)
	}
	return rule, nil
}

// validateGlob checks a user-supplied gitattributes pattern.
func validateGlob(g string, force bool) (string, error) {
	g = strings.TrimSpace(g)
	switch {
	case g == "":
		return "", fmt.Errorf("%w: empty glob", ErrInvalidArgument)
	case strings.ContainsAny(g, "\x00\n\r"):
		return "", fmt.Errorf("%w: glob %q contains a control character", ErrInvalidArgument, g)
	case strings.HasPrefix(g, "!"):
		return "", fmt.Errorf("%w: negative patterns are not allowed in .gitattributes: %q", ErrInvalidArgument, g)
	case strings.HasPrefix(g, "#"), strings.HasPrefix(g, "\""):
		return "", fmt.Errorf("%w: glob %q may not start with %q", ErrInvalidArgument, g, g[:1])
	case strings.ContainsAny(g, " \t") && strings.ContainsAny(g, `\"`):
		return "", fmt.Errorf("%w: glob %q mixes whitespace with backslash or quote; use a literal path instead", ErrInvalidArgument, g)
	}
	if !force {
		base := g
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		ext := strings.ToLower(filepath.Ext(base))
		if base == "*" || base == "**" || base == "" || sourceExtensions[ext] {
			return "", fmt.Errorf("%w: glob %q would capture source files; pass --force to track it anyway", ErrInvalidArgument, g)
		}
	}
	return g, nil
}

// rulesFor validates every selector entry into a rule string.
func (r *Repo) rulesFor(in TrackInput) ([]string, error) {
	if len(in.Patterns) == 0 {
		return nil, fmt.Errorf("%w: at least one path or --glob pattern is required", ErrInvalidArgument)
	}
	seen := map[string]bool{}
	var rules []string
	for _, p := range in.Patterns {
		var rule string
		var err error
		if in.Glob {
			rule, err = validateGlob(p, in.Force)
		} else {
			rule, err = LiteralRule(r.Root, p)
		}
		if err != nil {
			return nil, err
		}
		if !seen[rule] {
			seen[rule] = true
			rules = append(rules, rule)
		}
	}
	return rules, nil
}

// ---------------------------------------------------------------------------
// managed block parsing
// ---------------------------------------------------------------------------

// attrLine is one line of .gitattributes with its byte offset.
type attrLine struct {
	Offset int
	Text   string // without line terminator
}

func splitAttrLines(data []byte) []attrLine {
	var out []attrLine
	off := 0
	for off < len(data) {
		i := bytes.IndexByte(data[off:], '\n')
		var line []byte
		next := len(data)
		if i < 0 {
			line = data[off:]
		} else {
			line = data[off : off+i]
			next = off + i + 1
		}
		out = append(out, attrLine{Offset: off, Text: strings.TrimSuffix(string(line), "\r")})
		off = next
	}
	return out
}

// managedBlock is the parsed FXVCS block of .gitattributes.
type managedBlock struct {
	Found    bool
	Start    int // byte offsets of the block including markers
	End      int
	Patterns []string // pattern tokens as stored (quotes stripped)
	Raw      []byte   // exact block bytes
	// Foreign lists lines inside the block that fxvcs did not write (no
	// filter=fxvcs attribute): comments, other attributes, stray text.
	Foreign []string
}

func parseManagedBlock(data []byte) managedBlock {
	start := markerIndex(data, fivem.BlockStart, 0)
	if start < 0 {
		return managedBlock{}
	}
	endMarker := markerIndex(data, fivem.BlockEnd, start)
	end := len(data)
	if endMarker >= 0 {
		end = endMarker + len(fivem.BlockEnd)
		if end < len(data) && data[end] == '\r' {
			end++
		}
		if end < len(data) && data[end] == '\n' {
			end++
		}
	}
	mb := managedBlock{Found: true, Start: start, End: end, Raw: append([]byte(nil), data[start:end]...)}
	for _, l := range splitAttrLines(mb.Raw) {
		t := strings.TrimSpace(l.Text)
		if t == "" || t == fivem.BlockStart || t == fivem.BlockEnd {
			continue
		}
		if strings.HasPrefix(t, "#") || !strings.Contains(t, "filter=fxvcs") {
			mb.Foreign = append(mb.Foreign, t)
			continue
		}
		mb.Patterns = append(mb.Patterns, patternToken(t))
	}
	return mb
}

// markerIndex finds marker at the start of a line at or after from.
func markerIndex(data []byte, marker string, from int) int {
	off := from
	for off <= len(data) {
		i := bytes.Index(data[off:], []byte(marker))
		if i < 0 {
			return -1
		}
		pos := off + i
		after := pos + len(marker)
		if (pos == 0 || data[pos-1] == '\n') && (after == len(data) || data[after] == '\n' || data[after] == '\r') {
			return pos
		}
		off = pos + 1
	}
	return -1
}

// patternToken returns the pattern part of an attributes line: the C-quoted
// inner text when the line starts with a double quote, else the first field.
func patternToken(line string) string {
	if strings.HasPrefix(line, "\"") {
		for i := 1; i < len(line); i++ {
			if line[i] == '\\' {
				i++
				continue
			}
			if line[i] == '"' {
				return line[1:i]
			}
		}
		return line[1:]
	}
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i]
	}
	return line
}

// rawPatternToken is patternToken keeping the quotes (for reuse in a
// temporary attributes file).
func rawPatternToken(line string) string {
	if strings.HasPrefix(line, "\"") {
		return "\"" + patternToken(line) + "\""
	}
	return patternToken(line)
}

// ---------------------------------------------------------------------------
// block editing
// ---------------------------------------------------------------------------

func (r *Repo) attributesFile() string {
	return filepath.Join(r.Root, AttributesPath)
}

func readAttributes(path string) ([]byte, os.FileMode, error) {
	data, err := os.ReadFile(path) //nolint:gosec // repository root file
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0o644, nil
		}
		return nil, 0, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	return data, fi.Mode().Perm(), nil
}

// rewriteBlock replaces the managed block with the rendering of patterns
// (or removes it when patterns is empty) preserving every other byte. It
// returns whether the file changed.
func (r *Repo) rewriteBlock(ctx context.Context, edit func(existing []string) []string) (bool, error) {
	h, err := r.Lock(ctx, journal.LockCheckoutWorktree, "gitattributes")
	if err != nil {
		return false, err
	}
	defer func() { _ = h.Release() }()
	path := r.attributesFile()
	data, perm, err := readAttributes(path)
	if err != nil {
		return false, err
	}
	mb := parseManagedBlock(data)
	patterns := edit(mb.Patterns)
	var block []byte
	if len(patterns) > 0 {
		block = fivem.AttributesBlock(patterns)
	}
	out := fivem.MergeAttributes(data, block)
	if bytes.Equal(out, data) {
		return false, nil
	}
	// When the file held only our block the result is an empty file, not a
	// deletion: `git status` then shows exactly the removal of our lines.
	return true, writeFileAtomic(path, out, perm)
}

// Track adds rules to the managed block. It never rewrites history: the
// rules take effect for the next `git add`.
func (r *Repo) Track(ctx context.Context, in TrackInput) (TrackResult, error) {
	rules, err := r.rulesFor(in)
	if err != nil {
		return TrackResult{}, err
	}
	res := TrackResult{RulesAdded: []string{}, RulesPresent: []string{}}
	changed, err := r.rewriteBlock(ctx, func(existing []string) []string {
		have := map[string]bool{}
		for _, p := range existing {
			have[p] = true
		}
		out := append([]string(nil), existing...)
		for _, rule := range rules {
			if have[rule] {
				res.RulesPresent = append(res.RulesPresent, rule)
				continue
			}
			have[rule] = true
			out = append(out, rule)
			res.RulesAdded = append(res.RulesAdded, rule)
		}
		return out
	})
	if err != nil {
		return TrackResult{}, err
	}
	res.Changed = changed
	// Re-enabling a preset the repository had opted out of clears the opt-out
	// so upgrade keeps it current again.
	if in.Glob {
		if err := r.setPresetsDisabled(rules, false); err != nil {
			return res, err
		}
	}
	return res, nil
}

// Untrack removes rules from the managed block and reports index entries
// that are pointers no longer covered by any rule. It hydrates nothing.
func (r *Repo) Untrack(ctx context.Context, in TrackInput) (UntrackResult, error) {
	rules, err := r.rulesFor(in)
	if err != nil {
		return UntrackResult{}, err
	}
	res := UntrackResult{RulesRemoved: []string{}, RulesMissing: []string{}, PathsRequiringHydration: []string{}}
	// Snapshot which tracked paths are managed before the edit.
	tracked, err := r.lsFiles(ctx)
	if err != nil {
		return UntrackResult{}, err
	}
	before, err := r.Git.CheckAttrs(ctx, tracked)
	if err != nil {
		return UntrackResult{}, err
	}
	// A literal path without an exact rule that is still managed by another
	// rule cannot be untracked by removing lines: refuse with the covering
	// rule so the user can decide.
	mb := parseManagedBlock(readAttributesOrEmpty(r.attributesFile()))
	have := map[string]bool{}
	for _, p := range mb.Patterns {
		have[p] = true
	}
	if !in.Glob {
		for i, p := range in.Patterns {
			if have[rules[i]] {
				continue
			}
			chk, err := r.TrackingCheck(ctx, p)
			if err != nil {
				return UntrackResult{}, err
			}
			if chk.Tracked {
				return UntrackResult{}, fmt.Errorf("%w: %s matches %q (%s); remove that rule with: fxvcs untrack --glob %q", ErrRuleCovered, p, chk.MatchedRule, chk.RuleSource, chk.MatchedRule)
			}
		}
	}
	changed, err := r.rewriteBlock(ctx, func(existing []string) []string {
		remove := map[string]bool{}
		for _, rule := range rules {
			remove[rule] = true
		}
		var out []string
		removed := map[string]bool{}
		for _, p := range existing {
			if remove[p] {
				removed[p] = true
				continue
			}
			out = append(out, p)
		}
		for _, rule := range rules {
			if removed[rule] {
				res.RulesRemoved = append(res.RulesRemoved, rule)
			} else {
				res.RulesMissing = append(res.RulesMissing, rule)
			}
		}
		return out
	})
	if err != nil {
		return UntrackResult{}, err
	}
	res.Changed = changed
	// Opting out of a profile preset is recorded in the committed tracking
	// policy so `fxvcs upgrade` never re-adds it (reviewable like the block).
	if in.Glob {
		if err := r.setPresetsDisabled(rules, true); err != nil {
			return res, err
		}
	}
	if !changed {
		return res, nil
	}
	after, err := r.Git.CheckAttrs(ctx, tracked)
	if err != nil {
		return res, err
	}
	for _, p := range tracked {
		if !before[p].IsManaged() || after[p].IsManaged() {
			continue
		}
		blob, err := r.Git.CatFileBlob(ctx, ":"+p)
		if err != nil {
			return res, err
		}
		if pointer.Sniff(blob) {
			res.PathsRequiringHydration = append(res.PathsRequiringHydration, p)
		}
	}
	sort.Strings(res.PathsRequiringHydration)
	return res, nil
}

func readAttributesOrEmpty(path string) []byte {
	data, _, err := readAttributes(path)
	if err != nil {
		return nil
	}
	return data
}

// lsFiles returns every tracked path (index), NUL-safe.
func (r *Repo) lsFiles(ctx context.Context, extra ...string) ([]string, error) {
	args := append([]string{"ls-files", "-z"}, extra...)
	out, err := r.Git.Run(ctx, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range bytes.Split(out, []byte{0}) {
		if len(p) > 0 {
			paths = append(paths, string(p))
		}
	}
	return paths, nil
}

// ---------------------------------------------------------------------------
// tracking check
// ---------------------------------------------------------------------------

// TrackingCheck runs git check-attr for one repository-relative path and
// attributes the effective filter rule to a line of the root .gitattributes:
// every line that mentions the filter attribute is copied into a temporary
// attributes file under a unique marker attribute and evaluated by Git itself
// (core.attributesFile has top-level pattern semantics), so pattern matching
// is exactly Git's. The last matching line wins, as in Git.
func (r *Repo) TrackingCheck(ctx context.Context, rel string) (TrackingCheck, error) {
	p, err := pathsafe.Normalize(rel)
	if err != nil {
		return TrackingCheck{}, err
	}
	attrs, err := r.Git.CheckAttr(ctx, p)
	if err != nil {
		return TrackingCheck{}, err
	}
	res := TrackingCheck{
		File:          p,
		Tracked:       attrs.IsManaged(),
		RuleSource:    RuleSourceNone,
		AttributesRaw: fmt.Sprintf("filter=%s diff=%s merge=%s text=%s", orUnspecified(attrs.Filter), orUnspecified(attrs.Diff), orUnspecified(attrs.Merge), orUnspecified(attrs.Text)),
	}
	data := readAttributesOrEmpty(r.attributesFile())
	mb := parseManagedBlock(data)
	type cand struct {
		idx  int
		raw  string
		line attrLine
	}
	var cands []cand
	for i, l := range splitAttrLines(data) {
		t := strings.TrimSpace(l.Text)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[attr]") {
			continue
		}
		if !mentionsFilter(t) {
			continue
		}
		cands = append(cands, cand{idx: i, raw: rawPatternToken(t), line: l})
	}
	if len(cands) == 0 {
		if res.Tracked {
			res.RuleSource = RuleSourceOther
		}
		return res, nil
	}
	tmp, err := os.CreateTemp("", "fxvcs-attr-*")
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp.Name())
	var b strings.Builder
	names := make([]string, 0, len(cands))
	for _, c := range cands {
		name := "fxvcsline" + strconv.Itoa(c.idx)
		names = append(names, name)
		b.WriteString(c.raw)
		b.WriteByte(' ')
		b.WriteString(name)
		b.WriteByte('\n')
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	args := append([]string{"-c", "core.attributesFile=" + tmp.Name(), "check-attr", "-z"}, names...)
	args = append(args, "--", p)
	out, err := r.Git.Run(ctx, args...)
	if err != nil {
		return res, err
	}
	set := map[string]bool{}
	fields := bytes.Split(out, []byte{0})
	for i := 0; i+2 < len(fields); i += 3 {
		if string(fields[i+2]) == "set" {
			set[string(fields[i+1])] = true
		}
	}
	winner := -1
	for i, n := range names {
		if set[n] {
			winner = i
		}
	}
	if winner < 0 {
		if res.Tracked {
			res.RuleSource = RuleSourceOther
		}
		return res, nil
	}
	c := cands[winner]
	res.MatchedRule = patternToken(strings.TrimSpace(c.line.Text))
	if mb.Found && c.line.Offset >= mb.Start && c.line.Offset < mb.End {
		res.RuleSource = RuleSourceBlock
	} else {
		res.RuleSource = RuleSourceUser
	}
	return res, nil
}

func orUnspecified(v string) string {
	if v == "" {
		return "unspecified"
	}
	return v
}

// mentionsFilter reports whether an attributes line sets, unsets, or
// unspecifies the filter attribute.
func mentionsFilter(line string) bool {
	rest := line
	if strings.HasPrefix(rest, "\"") {
		tok := patternToken(rest)
		rest = strings.TrimPrefix(rest, "\""+tok+"\"")
	} else if i := strings.IndexAny(rest, " \t"); i >= 0 {
		rest = rest[i:]
	} else {
		return false
	}
	for _, f := range strings.Fields(rest) {
		f = strings.TrimLeft(f, "-!")
		if f == "filter" || strings.HasPrefix(f, "filter=") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// scan
// ---------------------------------------------------------------------------

// Scan lists tracked and untracked (not ignored) working-tree files of at
// least minBytes with whether a managed rule already applies. It reads only.
func (r *Repo) Scan(ctx context.Context, minBytes int64) ([]ScanCandidate, error) {
	tracked, err := r.lsFiles(ctx)
	if err != nil {
		return nil, err
	}
	untracked, err := r.lsFiles(ctx, "-o", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []ScanCandidate
	for _, p := range append(tracked, untracked...) {
		if seen[p] {
			continue
		}
		seen[p] = true
		fi, err := os.Lstat(filepath.Join(r.Root, filepath.FromSlash(p)))
		if err != nil || !fi.Mode().IsRegular() || fi.Size() < minBytes {
			continue
		}
		out = append(out, ScanCandidate{Path: p, Bytes: fi.Size()})
	}
	paths := make([]string, len(out))
	for i, c := range out {
		paths[i] = c.Path
	}
	attrs, err := r.Git.CheckAttrs(ctx, paths)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Tracked = attrs[out[i].Path].IsManaged()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].Path < out[j].Path
	})
	if out == nil {
		out = []ScanCandidate{}
	}
	return out, nil
}

// isPreset reports whether pattern is one of the active profile's presets.
func (r *Repo) isPreset(pattern string) bool {
	if r.Config.Spec.Profile != ProfileFiveM {
		return false
	}
	for _, p := range fivem.DefaultBinaryPatterns {
		if p == pattern {
			return true
		}
	}
	return false
}

// setPresetsDisabled records (or clears) the preset opt-out for the given
// glob rules in .fxvcs/repository.yaml (spec.tracking.disabledPresets),
// editing only that field and leaving the file uncommitted for review.
// Non-preset patterns are ignored.
func (r *Repo) setPresetsDisabled(rules []string, disabled bool) error {
	var presets []string
	for _, rule := range rules {
		if r.isPreset(rule) {
			presets = append(presets, rule)
		}
	}
	if len(presets) == 0 {
		return nil
	}
	current := []string{}
	if r.Config.Spec.Tracking != nil {
		current = append(current, r.Config.Spec.Tracking.DisabledPresets...)
	}
	set := map[string]bool{}
	for _, p := range current {
		set[p] = true
	}
	changed := false
	for _, p := range presets {
		if set[p] != disabled {
			set[p] = disabled
			changed = true
		}
	}
	if !changed {
		return nil
	}
	var next []string
	for _, p := range fivem.DefaultBinaryPatterns {
		if set[p] {
			next = append(next, p)
		}
	}
	return r.writeDisabledPresets(next)
}

// writeDisabledPresets rewrites spec.tracking.disabledPresets in the
// committed configuration preserving the rest of the document.
func (r *Repo) writeDisabledPresets(next []string) error {
	path := filepath.Join(r.Root, filepath.FromSlash(repository.ConfigPath))
	data, err := os.ReadFile(path) //nolint:gosec // repository config inside the checkout
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if len(doc.Content) == 0 {
		return fmt.Errorf("%w: empty %s", ErrInvalidArgument, repository.ConfigPath)
	}
	spec := mappingValue(doc.Content[0], "spec")
	if spec == nil {
		return fmt.Errorf("%w: %s has no spec", ErrInvalidArgument, repository.ConfigPath)
	}
	str := func(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }
	tracking := mappingValue(spec, "tracking")
	if len(next) == 0 {
		// Drop the whole tracking mapping when nothing else lives there.
		if tracking != nil {
			removeMappingKey(spec, "tracking")
		}
	} else {
		if tracking == nil {
			tracking = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			spec.Content = append(spec.Content, str("tracking"), tracking)
		}
		seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, p := range next {
			seq.Content = append(seq.Content, str(p))
		}
		if v := mappingValue(tracking, "disabledPresets"); v != nil {
			*v = *seq
		} else {
			tracking.Content = append(tracking.Content, str("disabledPresets"), seq)
		}
	}
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	cfg, err := repository.LoadConfigBytes(b.Bytes())
	if err != nil {
		return err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, b.Bytes(), fi.Mode().Perm()); err != nil {
		return err
	}
	r.Config = cfg
	return nil
}

// removeMappingKey deletes key (and its value) from a mapping node.
func removeMappingKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
