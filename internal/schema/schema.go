// Package schema validates FXVCS documents against the embedded JSON Schemas
// (spec/schemas) and the semantic rules a schema cannot express, then decodes
// them into internal/domain types.
//
// Validation is fail-closed and layered, in this order:
//
//  1. envelope (apiVersion/kind) via format.ValidateEnvelope, so a newer or
//     unknown apiVersion is reported as the typed format error rather than as a
//     schema issue;
//  2. explicit-null guard for activation (YAML null is never a tri-state);
//  3. JSON Schema draft 2020-12 (unknown fields are rejected everywhere);
//  4. strict decode into the domain struct (unknown fields rejected again);
//  5. semantic checks: ordering references, cycles, identity uniqueness,
//     path normalization and containment, discriminated unions, digests.
//
// Every failure is an Issue with a JSON-pointer Path. Nothing here writes.
package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/language"
	"golang.org/x/text/message"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/internal/format"
	"fxvcs.dev/fxvcs/spec/schemas"
)

// Issue is one validation failure. Path is a JSON pointer into the document
// ("" for the whole document). Cause carries a typed sentinel when one exists
// (for example format.ErrNewerAPIVersion) so callers can errors.Is it.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}

// Error formats the issue as "<path>: <message>".
func (i Issue) Error() string {
	if i.Path == "" {
		return i.Message
	}
	return i.Path + ": " + i.Message
}

// Unwrap exposes the typed cause.
func (i Issue) Unwrap() error { return i.Cause }

// Issues is a non-empty list of issues; it implements error and unwraps to its
// members so errors.Is(err, format.ErrNewerAPIVersion) works.
type Issues []Issue

func (is Issues) Error() string {
	switch len(is) {
	case 0:
		return "schema: no issues"
	case 1:
		return "schema: " + is[0].Error()
	}
	return fmt.Sprintf("schema: %s (and %d more)", is[0].Error(), len(is)-1)
}

// Unwrap returns the members as errors.
func (is Issues) Unwrap() []error {
	out := make([]error, len(is))
	for i := range is {
		out[i] = is[i]
	}
	return out
}

// Kinds of schema-only documents that carry no committed envelope. These
// version themselves independently of apiVersion.
const (
	KindPointer           = "Pointer"
	KindObjectManifest    = "ObjectManifest"
	KindAssetIndex        = "AssetIndex"
	KindHydrationStateRow = "HydrationStateRow"
	KindPublicationLedger = "PublicationLedger"
)

// PointerDocument is the decoded field view of a v1 pointer, matching
// pointer-v1.schema.json. internal/pointer owns the text encoding.
type PointerDocument struct {
	Version  int    `json:"version"`
	Domain   string `json:"domain"`
	Encoding string `json:"encoding"`
	Manifest string `json:"manifest"`
	OID      string `json:"oid"`
	Size     int64  `json:"size"`
}

// kindSpec binds a document kind to its schema, decode target and semantic
// checks. newValue returns a pointer to a fresh Go value, or nil when the kind
// has no Go representation (schema-only validation).
type kindSpec struct {
	kind     string
	file     string // path within schemas.FS
	envelope bool
	newValue func() any
	semantic func(v any) Issues
}

var registry = []kindSpec{
	{format.KindRepository, "v1alpha1/repository.schema.json", true, func() any { return new(domain.Repository) }, func(v any) Issues { return checkRepository(v.(*domain.Repository)) }},
	{format.KindResourceCatalog, "v1alpha1/resource-catalog.schema.json", true, func() any { return new(domain.ResourceCatalog) }, func(v any) Issues { return checkResourceCatalog(v.(*domain.ResourceCatalog)) }},
	{format.KindRuntimeResourceCatalog, "v1alpha1/runtime-resource-catalog.schema.json", true, func() any { return new(domain.RuntimeResourceCatalog) }, func(v any) Issues { return checkRuntimeResourceCatalog(v.(*domain.RuntimeResourceCatalog)) }},
	{format.KindResourceOverrides, "v1alpha1/resource-overrides.schema.json", true, func() any { return new(domain.ResourceOverrides) }, func(v any) Issues { return checkResourceOverrides(v.(*domain.ResourceOverrides)) }},
	{format.KindStack, "v1alpha1/stack.schema.json", true, func() any { return new(domain.Stack) }, func(v any) Issues { return checkStack(v.(*domain.Stack)) }},
	{format.KindRelease, "v1alpha1/release.schema.json", true, func() any { return new(domain.Release) }, func(v any) Issues { return checkRelease(v.(*domain.Release)) }},
	{format.KindEnvironment, "v1alpha1/environment.schema.json", true, func() any { return new(domain.Environment) }, func(v any) Issues { return checkEnvironment(v.(*domain.Environment)) }},
	{format.KindTarget, "v1alpha1/target.schema.json", true, func() any { return new(domain.Target) }, func(v any) Issues { return checkTarget(v.(*domain.Target)) }},
	{format.KindAgentConfig, "v1alpha1/agent-config.schema.json", true, func() any { return new(domain.AgentConfig) }, func(v any) Issues { return checkAgentConfig(v.(*domain.AgentConfig)) }},
	{KindPointer, "pointer-v1.schema.json", false, func() any { return new(PointerDocument) }, func(v any) Issues { return checkPointer(v.(*PointerDocument)) }},
	{KindObjectManifest, "object-manifest-v1.schema.json", false, func() any { return new(domain.ObjectManifest) }, func(v any) Issues { return checkObjectManifest(v.(*domain.ObjectManifest)) }},
	{KindAssetIndex, "asset-index-v1.schema.json", false, func() any { return new(domain.AssetIndex) }, func(v any) Issues { return checkAssetIndex(v.(*domain.AssetIndex)) }},
	{KindHydrationStateRow, "hydration-state-v1.schema.json", false, nil, nil},
	{KindPublicationLedger, "publication-ledger-v1.schema.json", false, nil, nil},
}

// InputSourceSchema is the standalone schema file for the hook input union;
// it is validated as part of Target but exposed for tooling.
const InputSourceSchema = "v1alpha1/input-source.schema.json"

func lookup(kind string) (kindSpec, bool) {
	for _, k := range registry {
		if k.kind == kind {
			return k, true
		}
	}
	return kindSpec{}, false
}

// KnownKinds returns every kind this package can validate, envelope kinds
// first, in registration order.
func KnownKinds() []string {
	out := make([]string, 0, len(registry))
	for _, k := range registry {
		out = append(out, k.kind)
	}
	return out
}

// SchemaFile returns the schema path (relative to spec/schemas) for a kind.
func SchemaFile(kind string) (string, bool) {
	k, ok := lookup(kind)
	return k.file, ok
}

// SchemaJSON returns the raw embedded schema bytes for a kind.
func SchemaJSON(kind string) ([]byte, error) {
	k, ok := lookup(kind)
	if !ok {
		return nil, fmt.Errorf("schema: unknown kind %q", kind)
	}
	return schemas.FS.ReadFile(k.file)
}

// ---------------------------------------------------------------------------
// Compilation
// ---------------------------------------------------------------------------

type compiledSet struct {
	byKind map[string]*jsonschema.Schema
	raw    map[string]map[string]any // $id URL -> raw schema document
}

var (
	compileOnce sync.Once
	compiledAll *compiledSet
	compileErr  error
)

// Compile compiles every embedded schema once and reports the first error.
// Tests call it directly; Validate calls it lazily.
func Compile() error {
	_, err := compiled()
	return err
}

func compiled() (*compiledSet, error) {
	compileOnce.Do(func() {
		compiledAll, compileErr = compileAll()
	})
	return compiledAll, compileErr
}

func compileAll() (*compiledSet, error) {
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	set := &compiledSet{byKind: map[string]*jsonschema.Schema{}, raw: map[string]map[string]any{}}
	err := fs.WalkDir(schemas.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".schema.json") {
			return nil
		}
		data, err := schemas.FS.ReadFile(path)
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return fmt.Errorf("schema %s: %w", path, err)
		}
		m, ok := doc.(map[string]any)
		if !ok {
			return fmt.Errorf("schema %s: not an object", path)
		}
		url := schemas.BaseURL + path
		if id, _ := m["$id"].(string); id != url {
			return fmt.Errorf("schema %s: $id %q must equal %q", path, id, url)
		}
		set.raw[url] = m
		return c.AddResource(url, doc)
	})
	if err != nil {
		return nil, err
	}
	for _, k := range registry {
		sch, err := c.Compile(schemas.BaseURL + k.file)
		if err != nil {
			return nil, fmt.Errorf("schema %s: %w", k.file, err)
		}
		set.byKind[k.kind] = sch
	}
	// Standalone compile of the input-source schema proves it is self-consistent.
	if _, err := c.Compile(schemas.BaseURL + InputSourceSchema); err != nil {
		return nil, fmt.Errorf("schema %s: %w", InputSourceSchema, err)
	}
	return set, nil
}

// RawSchema returns the parsed (uncompiled) schema document for a $id URL or
// schema path. Used by drift tests to compare Go struct fields with schema
// properties.
func RawSchema(idOrPath string) (map[string]any, error) {
	set, err := compiled()
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(idOrPath, schemas.BaseURL) {
		idOrPath = schemas.BaseURL + idOrPath
	}
	m, ok := set.raw[idOrPath]
	if !ok {
		return nil, fmt.Errorf("schema: no schema %q", idOrPath)
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Public validation entry points
// ---------------------------------------------------------------------------

// Validate parses data (YAML or JSON) and validates it as kind. kind may be ""
// for committed documents; it is then taken from the document's envelope.
func Validate(kind string, data []byte) Issues {
	doc, iss := ParseYAML(data)
	if len(iss) > 0 {
		return iss
	}
	return ValidateDocument(kind, doc)
}

// ValidateDocument validates an already-parsed generic document
// (map[string]any / []any / string / int64 / bool / nil).
func ValidateDocument(kind string, doc any) Issues {
	_, iss := load(kind, doc)
	return iss
}

// Load parses, validates and decodes data. On success it returns a pointer to
// the domain type for kind (see KnownKinds; schema-only kinds return the
// generic map). The error, when non-nil, is always Issues.
func Load(kind string, data []byte) (any, error) {
	doc, iss := ParseYAML(data)
	if len(iss) > 0 {
		return nil, iss
	}
	return LoadDocument(kind, doc)
}

// LoadDocument is Load for an already-parsed generic document.
func LoadDocument(kind string, doc any) (any, error) {
	v, iss := load(kind, doc)
	if len(iss) > 0 {
		return nil, iss
	}
	return v, nil
}

// DetectKind returns the envelope of a committed document without validating
// it. It fails only when the document is not a mapping or lacks the fields.
func DetectKind(data []byte) (format.Envelope, error) {
	doc, iss := ParseYAML(data)
	if len(iss) > 0 {
		return format.Envelope{}, iss
	}
	m, ok := doc.(map[string]any)
	if !ok {
		return format.Envelope{}, Issues{{Message: "document must be a mapping"}}
	}
	env := format.Envelope{}
	env.APIVersion, _ = m["apiVersion"].(string)
	env.Kind, _ = m["kind"].(string)
	if env.Kind == "" {
		return env, Issues{{Path: "/kind", Message: "missing property 'kind'"}}
	}
	return env, nil
}

// Typed loaders.

func LoadRepository(data []byte) (*domain.Repository, error) {
	v, err := Load(format.KindRepository, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.Repository), nil
}

func LoadResourceCatalog(data []byte) (*domain.ResourceCatalog, error) {
	v, err := Load(format.KindResourceCatalog, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.ResourceCatalog), nil
}

func LoadRuntimeResourceCatalog(data []byte) (*domain.RuntimeResourceCatalog, error) {
	v, err := Load(format.KindRuntimeResourceCatalog, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.RuntimeResourceCatalog), nil
}

func LoadResourceOverrides(data []byte) (*domain.ResourceOverrides, error) {
	v, err := Load(format.KindResourceOverrides, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.ResourceOverrides), nil
}

func LoadStack(data []byte) (*domain.Stack, error) {
	v, err := Load(format.KindStack, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.Stack), nil
}

func LoadRelease(data []byte) (*domain.Release, error) {
	v, err := Load(format.KindRelease, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.Release), nil
}

func LoadEnvironment(data []byte) (*domain.Environment, error) {
	v, err := Load(format.KindEnvironment, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.Environment), nil
}

func LoadTarget(data []byte) (*domain.Target, error) {
	v, err := Load(format.KindTarget, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.Target), nil
}

func LoadAgentConfig(data []byte) (*domain.AgentConfig, error) {
	v, err := Load(format.KindAgentConfig, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.AgentConfig), nil
}

func LoadObjectManifest(data []byte) (*domain.ObjectManifest, error) {
	v, err := Load(KindObjectManifest, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.ObjectManifest), nil
}

func LoadAssetIndex(data []byte) (*domain.AssetIndex, error) {
	v, err := Load(KindAssetIndex, data)
	if err != nil {
		return nil, err
	}
	return v.(*domain.AssetIndex), nil
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

func load(kind string, doc any) (any, Issues) {
	m, ok := doc.(map[string]any)
	if !ok {
		return nil, Issues{{Message: "document must be a mapping"}}
	}
	if kind == "" {
		k, present := m["kind"]
		if !present {
			return nil, Issues{{Path: "/kind", Message: "missing property 'kind'"}}
		}
		ks, _ := k.(string)
		if _, known := lookup(ks); !known || ks == "" {
			return nil, Issues{{Path: "/kind", Message: fmt.Sprintf("unknown kind %q", ks), Cause: format.ErrUnknownKind}}
		}
		kind = ks
	}
	spec, ok := lookup(kind)
	if !ok {
		return nil, Issues{{Path: "/kind", Message: fmt.Sprintf("unknown kind %q", kind), Cause: format.ErrUnknownKind}}
	}

	// 1. envelope
	if spec.envelope {
		if iss := checkEnvelope(m, kind); len(iss) > 0 {
			return nil, iss
		}
	}
	// 2. explicit-null guard
	if iss := checkNullActivation("", m); len(iss) > 0 {
		return nil, iss
	}
	// 3. JSON schema
	set, err := compiled()
	if err != nil {
		return nil, Issues{{Message: "schema compile: " + err.Error(), Cause: err}}
	}
	if err := set.byKind[kind].Validate(m); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			if iss := flatten(ve); len(iss) > 0 {
				return nil, iss
			}
		}
		return nil, Issues{{Message: err.Error(), Cause: err}}
	}
	if spec.newValue == nil {
		return m, nil
	}
	// 4. strict decode
	v := spec.newValue()
	buf, err := json.Marshal(m)
	if err != nil {
		return nil, Issues{{Message: "encode: " + err.Error(), Cause: err}}
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, Issues{{Message: "decode: " + err.Error(), Cause: err}}
	}
	// 5. semantic
	if spec.semantic != nil {
		if iss := spec.semantic(v); len(iss) > 0 {
			return nil, iss
		}
	}
	return v, nil
}

func checkEnvelope(m map[string]any, kind string) Issues {
	var env format.Envelope
	av, hasAPI := m["apiVersion"]
	kv, hasKind := m["kind"]
	if !hasAPI {
		return Issues{{Path: "/apiVersion", Message: "missing property 'apiVersion'"}}
	}
	if !hasKind {
		return Issues{{Path: "/kind", Message: "missing property 'kind'"}}
	}
	var ok bool
	if env.APIVersion, ok = av.(string); !ok {
		return Issues{{Path: "/apiVersion", Message: "apiVersion must be a string"}}
	}
	if env.Kind, ok = kv.(string); !ok {
		return Issues{{Path: "/kind", Message: "kind must be a string"}}
	}
	if err := format.ValidateEnvelope(env, kind); err != nil {
		path := "/apiVersion"
		if errors.Is(err, format.ErrUnknownKind) {
			path = "/kind"
		}
		return Issues{{Path: path, Message: err.Error(), Cause: err}}
	}
	return nil
}

// checkNullActivation rejects `activation: null` anywhere in the document.
// The schema would also reject it, but the message here names the rule.
func checkNullActivation(path string, v any) Issues {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + "/" + escapePointer(k)
			if k == "activation" && t[k] == nil {
				return Issues{{Path: p, Message: "activation must be one of \"enabled\", \"disabled\", \"pending\"; YAML null and missing keys are forbidden"}}
			}
			if iss := checkNullActivation(p, t[k]); len(iss) > 0 {
				return iss
			}
		}
	case []any:
		for i, e := range t {
			if iss := checkNullActivation(fmt.Sprintf("%s/%d", path, i), e); len(iss) > 0 {
				return iss
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// jsonschema error flattening
// ---------------------------------------------------------------------------

var printer = message.NewPrinter(language.English)

func flatten(ve *jsonschema.ValidationError) Issues {
	var out Issues
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		switch k := e.ErrorKind.(type) {
		case *kind.OneOf, *kind.AnyOf:
			out = append(out, Issue{Path: pointerOf(e.InstanceLocation), Message: unionMessage(e)})
			return
		case *kind.FalseSchema:
			out = append(out, Issue{Path: pointerOf(e.InstanceLocation), Message: "field is not allowed in this context"})
			return
		case *kind.Schema, *kind.Reference, *kind.Group, *kind.AllOf:
			// structural: descend
		default:
			if len(e.Causes) == 0 {
				out = append(out, Issue{Path: pointerOf(e.InstanceLocation), Message: k.LocalizedString(printer)})
				return
			}
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

func unionMessage(e *jsonschema.ValidationError) string {
	var kindName string
	switch k := e.ErrorKind.(type) {
	case *kind.OneOf:
		if len(k.Subschemas) > 1 {
			return fmt.Sprintf("value must match exactly one variant, but matched %d", len(k.Subschemas))
		}
		kindName = "exactly one"
	case *kind.AnyOf:
		kindName = "at least one"
	}
	seen := map[string]bool{}
	var parts []string
	var leaves func(c *jsonschema.ValidationError)
	leaves = func(c *jsonschema.ValidationError) {
		if len(c.Causes) == 0 {
			if _, ok := c.ErrorKind.(*kind.Schema); ok {
				return
			}
			s := c.ErrorKind.LocalizedString(printer)
			if !seen[s] {
				seen[s] = true
				parts = append(parts, s)
			}
			return
		}
		for _, cc := range c.Causes {
			leaves(cc)
		}
	}
	for _, c := range e.Causes {
		leaves(c)
	}
	msg := "value must match " + kindName + " allowed variant"
	if len(parts) > 0 {
		msg += " (" + strings.Join(parts, "; ") + ")"
	}
	return msg
}

func pointerOf(tokens []string) string {
	if len(tokens) == 0 {
		return ""
	}
	var b strings.Builder
	for _, t := range tokens {
		b.WriteByte('/')
		b.WriteString(escapePointer(t))
	}
	return b.String()
}

func escapePointer(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	return strings.ReplaceAll(s, "/", "~1")
}
