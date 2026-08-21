// Package fivem is the FiveM profile: default tracking presets, the managed
// .gitattributes block, and change classification. Manifest evaluation,
// discovery, and catalog maintenance live in its subpackages.
package fivem

import (
	"bytes"
	"strings"
)

// DefaultBinaryPatterns are the tracking candidates for real binary FiveM
// formats: RAGE map/model/texture/collision/animation/navmesh archives, audio,
// packaged archives, Scaleform, and common image/audio containers used by NUI
// resources. Source files (.lua, .js, .ts, .json, .cfg, .meta, .xml, .html,
// .css) are text and stay ordinary Git blobs unless explicitly selected.
var DefaultBinaryPatterns = []string{
	// RAGE / RSC7 stream and data resources
	"*.ytd", "*.ydr", "*.yft", "*.ybn", "*.ymap", "*.ytyp", "*.ydd", "*.ycd",
	"*.ynv", "*.yed", "*.yld", "*.ypt", "*.ypdb", "*.ymt", "*.ynd", "*.ymf",
	"*.yvr", "*.ywr", "*.yfd", "*.ysc", "*.mrf", "*.gxt2",
	// audio, video, archives, Scaleform
	"*.awc", "*.rel", "*.rpf", "*.oiv", "*.gfx", "*.bk2",
	// NUI media and fonts
	"*.png", "*.jpg", "*.jpeg", "*.dds", "*.webp", "*.gif", "*.ico", "*.bmp", "*.tga",
	"*.ttf", "*.otf", "*.woff", "*.woff2",
	"*.ogg", "*.mp3", "*.wav", "*.flac", "*.aac", "*.m4a", "*.webm", "*.mp4",
	// compiled code shipped inside resources (C# / native modules)
	"*.dll", "*.pdb", "*.so", "*.node", "*.wasm",
	// vendored archives
	"*.zip", "*.7z", "*.gz",
}

// Managed block markers. Everything between them is owned by FXVCS; lines
// outside are never touched.
const (
	BlockStart = "# >>> fxvcs managed (do not edit) >>>"
	BlockEnd   = "# <<< fxvcs managed <<<"
	// AttributeSuffix is appended to every managed pattern in v1.
	AttributeSuffix = "filter=fxvcs diff=fxvcs merge=fxvcs -text"
)

// AttributesBlock renders the delimited managed block for patterns, one line
// per pattern, terminated by a newline. Patterns are emitted in the given
// order; duplicates and blank patterns are dropped. Patterns containing
// whitespace are quoted as Git requires.
func AttributesBlock(patterns []string) []byte {
	var b bytes.Buffer
	b.WriteString(BlockStart)
	b.WriteByte('\n')
	seen := map[string]bool{}
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if strings.ContainsAny(p, " \t") {
			p = "\"" + p + "\""
		}
		b.WriteString(p)
		b.WriteByte(' ')
		b.WriteString(AttributeSuffix)
		b.WriteByte('\n')
	}
	b.WriteString(BlockEnd)
	b.WriteByte('\n')
	return b.Bytes()
}

// MergeAttributes returns existing with its managed block replaced by block.
// Unrelated lines are preserved byte-for-byte (including line endings and a
// missing trailing newline outside the block). When no block exists it is
// appended, separated by a newline if the file does not already end with one.
// A start marker without an end marker is treated as extending to the end of
// the file so a truncated block cannot leak into user lines. block should
// come from AttributesBlock; an empty block removes the managed section.
func MergeAttributes(existing []byte, block []byte) []byte {
	start, end, found := findBlock(existing)
	if !found {
		if len(block) == 0 {
			return append([]byte(nil), existing...)
		}
		out := append([]byte(nil), existing...)
		if len(out) > 0 && out[len(out)-1] != '\n' {
			out = append(out, '\n')
		}
		return append(out, block...)
	}
	out := make([]byte, 0, len(existing)-(end-start)+len(block))
	out = append(out, existing[:start]...)
	out = append(out, block...)
	out = append(out, existing[end:]...)
	return out
}

// findBlock locates [start,end) byte offsets of the managed block including
// its markers and the end marker's line terminator.
func findBlock(data []byte) (int, int, bool) {
	start := lineIndex(data, BlockStart, 0)
	if start < 0 {
		return 0, 0, false
	}
	endMarker := lineIndex(data, BlockEnd, start)
	if endMarker < 0 {
		return start, len(data), true
	}
	end := endMarker + len(BlockEnd)
	// swallow one line terminator (\n or \r\n)
	if end < len(data) && data[end] == '\r' {
		end++
	}
	if end < len(data) && data[end] == '\n' {
		end++
	}
	return start, end, true
}

// lineIndex finds marker at the start of a line (ignoring a trailing \r) at
// or after from.
func lineIndex(data []byte, marker string, from int) int {
	off := from
	for off <= len(data) {
		i := bytes.Index(data[off:], []byte(marker))
		if i < 0 {
			return -1
		}
		pos := off + i
		atLineStart := pos == 0 || data[pos-1] == '\n'
		after := pos + len(marker)
		atLineEnd := after == len(data) || data[after] == '\n' || data[after] == '\r'
		if atLineStart && atLineEnd {
			return pos
		}
		off = pos + 1
	}
	return -1
}
