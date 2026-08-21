package cli

import (
	"regexp"
	"strings"
)

// The engine speaks in its own terms (pointer, ledger, index, hydrate). Human
// output speaks FiveM (placeholder, upload records, committed files, load).
// humanize rewrites an engine message with the docs/cli-ux.md vocabulary;
// --verbose shows the original text.

// phraseRewrites are tried first, longest and most specific phrases before
// generic words. Order matters.
var phraseRewrites = []struct{ from, to string }{
	{"not in the index or not managed by fxvcs (filter=fxvcs)", "not tracked by fxvcs"},
	{"path is not managed by fxvcs", "path is not tracked by fxvcs"},
	{"is not an fxvcs pointer (not migrated); run fxvcs migrate", "is an ordinary committed file, not yet moved into fxvcs storage; run fxvcs migrate"},
	{"working file is modified (hydrated-modified); commit it with git add or discard the change first", "changed locally; commit it with git add/commit or discard the change first"},
	{"working file is modified (hydrated-modified); commit it with git add or discard the change; repair never overwrites it", "changed locally; commit it with git add/commit or discard the change (repair never overwrites your files)"},
	{"working file is modified (hydrated-modified); never overwritten — commit it with git add or discard the change", "changed locally; commit it with git add/commit or discard the change (repair never overwrites your files)"},
	{"working file holds a pointer to a different object than the index (conflict); run fxvcs doctor", "needs attention: your file and the committed version disagree; run fxvcs doctor"},
	{"path is unmerged; resolve the conflict first (git checkout --ours/--theirs, git add)", "needs attention: unresolved merge conflict; resolve it with git first (git checkout --ours/--theirs, git add)"},
	{"unmerged index entry (conflict)", "unresolved merge conflict (needs attention)"},
	{"invalid index pointer (conflict)", "the committed placeholder is invalid (needs attention)"},
	{"index pointer is invalid", "the committed placeholder is invalid"},
	{"already hydrated", "already loaded"},
	{"already a pointer", "already unloaded"},
	{"is not in the local cache", "is not available on this computer"},
	{"publication ledger row", "upload record"},
	{"ledger row", "upload record"},
	{"publication ledger", "upload records"},
	{"the ledger", "the upload records"},
	{"pending publication", "not yet uploaded"},
	{"requiredForPublication remote", "storage required before push"},
	{"requiredForPublication", "required before push"},
	{"required remote", "storage required before push"},
	{"storage remote", "storage"},
	{"remote has no local binding on this machine", "storage is not set up on this computer"},
	{"has no local binding on this machine", "is not set up on this computer"},
	{"hydrated working file", "loaded file"},
	{"hydrated-modified", "changed locally"},
	{"unhydrated", "not loaded"},
	{"dehydrated", "unloaded"},
	{"hydrated", "loaded"},
	{"dehydrate", "unload"},
	{"hydration", "loading"},
	{"hydrate", "load"},
	{"index pointer", "committed placeholder"},
	{"index entries", "committed files"},
	{"index entry", "committed file"},
	{"pointer bytes", "placeholder"},
	{"pointers", "placeholders"},
	{"pointer", "placeholder"},
	{"filter=fxvcs", "tracked by fxvcs"},
	{"spec.profile", "profile"},
	{"storageDomainID", "storage id"},
	{"storageDomain", "storage"},
	{"repositoryID", "repository id"},
	{"managed by fxvcs", "tracked by fxvcs"},
	{"FXVCS block", "fxvcs section of .gitattributes"},
	{"the index", "the committed version"},
}

var (
	appPrefix   = regexp.MustCompile(`\b(app|controlplane|stack|release|gitops): `)
	digestRE    = regexp.MustCompile(`\bobject sha256:[0-9a-f]{6,}`)
	bareDigest  = regexp.MustCompile(`\bsha256:[0-9a-f]{6,}`)
	multiSpaces = regexp.MustCompile(`  +`)
)

// humanize rewrites an engine or SDK message into the human vocabulary.
func humanize(s string) string {
	s = appPrefix.ReplaceAllString(s, "")
	for _, r := range phraseRewrites {
		s = strings.ReplaceAll(s, r.from, r.to)
	}
	s = digestRE.ReplaceAllString(s, "asset data")
	s = bareDigest.ReplaceAllString(s, "asset data")
	s = multiSpaces.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// humanReason picks the message to show for a per-item reason.
func humanReason(verbose bool, reason string) string {
	if verbose {
		return reason
	}
	return humanize(reason)
}
