// Package schemas embeds the normative JSON Schema (draft 2020-12) documents
// and the pointer ABNF grammar so that the binary validates against exactly the
// files published under spec/schemas. internal/schema compiles them; nothing
// else should read the files at runtime.
package schemas

import "embed"

// FS contains every schema file, keyed by its path relative to spec/schemas
// (for example "v1alpha1/repository.schema.json" or "pointer-v1.schema.json").
//
//go:embed *.schema.json v1alpha1/*.schema.json pointer.abnf
var FS embed.FS

// BaseURL is the $id prefix shared by every embedded schema; a file at path p
// has $id BaseURL+p.
const BaseURL = "https://fxvcs.dev/schemas/"
