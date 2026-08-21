package schema

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"fxvcs.dev/fxvcs/internal/domain"
	"fxvcs.dev/fxvcs/spec/schemas"
)

// TestStructFieldsMatchSchemaProperties walks every domain type reachable from
// each document root alongside the schema and requires that the set of Go
// `json` field names equals the set of schema `properties` keys at every
// object level, and that scalar types agree. This catches drift in both
// directions: a field added to Go without a schema entry, or a schema property
// with no Go field.
func TestStructFieldsMatchSchemaProperties(t *testing.T) {
	if err := Compile(); err != nil {
		t.Fatal(err)
	}
	roots := []struct {
		kind string
		typ  reflect.Type
	}{
		{"Repository", reflect.TypeOf(domain.Repository{})},
		{"ResourceCatalog", reflect.TypeOf(domain.ResourceCatalog{})},
		{"RuntimeResourceCatalog", reflect.TypeOf(domain.RuntimeResourceCatalog{})},
		{"ResourceOverrides", reflect.TypeOf(domain.ResourceOverrides{})},
		{"Stack", reflect.TypeOf(domain.Stack{})},
		{"Release", reflect.TypeOf(domain.Release{})},
		{"Environment", reflect.TypeOf(domain.Environment{})},
		{"Target", reflect.TypeOf(domain.Target{})},
		{"AgentConfig", reflect.TypeOf(domain.AgentConfig{})},
		{KindPointer, reflect.TypeOf(PointerDocument{})},
		{KindObjectManifest, reflect.TypeOf(domain.ObjectManifest{})},
		{KindAssetIndex, reflect.TypeOf(domain.AssetIndex{})},
	}
	for _, r := range roots {
		t.Run(r.kind, func(t *testing.T) {
			file, ok := SchemaFile(r.kind)
			if !ok {
				t.Fatal("no schema")
			}
			w := &driftWalker{t: t, seen: map[string]bool{}}
			doc, err := RawSchema(file)
			if err != nil {
				t.Fatal(err)
			}
			w.walk(r.typ, schemaRef{base: schemas.BaseURL + file, node: doc}, r.kind)
			if w.objects == 0 {
				t.Fatal("walker visited no objects")
			}
		})
	}
	// The InputSource union is reached through Target; make sure it was.
	w := &driftWalker{t: t, seen: map[string]bool{}}
	doc, err := RawSchema(InputSourceSchema)
	if err != nil {
		t.Fatal(err)
	}
	w.walk(reflect.TypeOf(domain.InputSource{}), schemaRef{base: schemas.BaseURL + InputSourceSchema, node: doc}, "InputSource")
}

type schemaRef struct {
	base string // $id of the document containing node
	node map[string]any
}

type schemaView struct {
	properties map[string]schemaRef
	items      *schemaRef
	additional *schemaRef
	types      map[string]bool
	hasConst   bool
	hasEnum    bool
}

type driftWalker struct {
	t       *testing.T
	seen    map[string]bool
	objects int
}

func (w *driftWalker) walk(typ reflect.Type, ref schemaRef, path string) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if ref.node == nil {
		w.t.Errorf("%s: no schema node for Go type %s", path, typ)
		return
	}
	view := w.collect(ref, map[string]bool{})
	switch typ.Kind() {
	case reflect.Struct:
		w.objects++
		fields := jsonFields(typ)
		key := typ.String() + "@" + path
		if w.seen[key] {
			return
		}
		w.seen[key] = true
		var goNames, schemaNames []string
		for name := range fields {
			goNames = append(goNames, name)
		}
		for name := range view.properties {
			schemaNames = append(schemaNames, name)
		}
		sort.Strings(goNames)
		sort.Strings(schemaNames)
		if strings.Join(goNames, ",") != strings.Join(schemaNames, ",") {
			w.t.Errorf("%s (%s): Go json fields %v != schema properties %v", path, typ, goNames, schemaNames)
			return
		}
		for name, ft := range fields {
			w.walk(ft, view.properties[name], path+"."+name)
		}
	case reflect.Slice:
		if !view.types["array"] {
			w.t.Errorf("%s: Go slice but schema type %v", path, keys(view.types))
			return
		}
		if view.items == nil {
			w.t.Errorf("%s: schema array without items", path)
			return
		}
		w.walk(typ.Elem(), *view.items, path+"[]")
	case reflect.Map:
		if !view.types["object"] {
			w.t.Errorf("%s: Go map but schema type %v", path, keys(view.types))
			return
		}
		if view.additional == nil {
			w.t.Errorf("%s: schema object for Go map without additionalProperties schema", path)
			return
		}
		if len(view.properties) > 0 {
			w.t.Errorf("%s: schema declares properties but Go type is a map", path)
		}
		w.walk(typ.Elem(), *view.additional, path+"{}")
	case reflect.String:
		if len(view.types) > 0 && !view.types["string"] {
			w.t.Errorf("%s: Go string but schema type %v", path, keys(view.types))
		}
		if len(view.types) == 0 && !view.hasConst && !view.hasEnum {
			w.t.Errorf("%s: schema for Go string has no type/const/enum", path)
		}
	case reflect.Bool:
		if !view.types["boolean"] {
			w.t.Errorf("%s: Go bool but schema type %v", path, keys(view.types))
		}
	case reflect.Int, reflect.Int64, reflect.Int32:
		if !view.types["integer"] {
			w.t.Errorf("%s: Go integer but schema type %v", path, keys(view.types))
		}
	default:
		w.t.Errorf("%s: unsupported Go kind %s", path, typ.Kind())
	}
}

// collect merges properties/items/additionalProperties/type from a node, its
// $ref target and every allOf member (recursively). Conditional keywords
// (if/then/else, oneOf, anyOf) never introduce properties in FXVCS schemas.
func (w *driftWalker) collect(ref schemaRef, visiting map[string]bool) schemaView {
	v := schemaView{properties: map[string]schemaRef{}, types: map[string]bool{}}
	var merge func(r schemaRef)
	merge = func(r schemaRef) {
		n := r.node
		if n == nil {
			return
		}
		if rs, ok := n["$ref"].(string); ok {
			target, err := w.resolveRef(r.base, rs)
			if err != nil {
				w.t.Fatalf("resolve %s from %s: %v", rs, r.base, err)
			}
			k := target.base + "#" + fmt.Sprint(len(visiting))
			if !visiting[k] {
				visiting[k] = true
				merge(target)
			}
		}
		if props, ok := n["properties"].(map[string]any); ok {
			for name, sub := range props {
				if sm, ok := sub.(map[string]any); ok {
					v.properties[name] = schemaRef{base: r.base, node: sm}
				} else if b, ok := sub.(bool); ok && !b {
					// property forbidden here (false schema): still a declared name
					v.properties[name] = schemaRef{base: r.base, node: map[string]any{"type": "null"}}
				}
			}
		}
		if it, ok := n["items"].(map[string]any); ok {
			v.items = &schemaRef{base: r.base, node: it}
		}
		if ap, ok := n["additionalProperties"].(map[string]any); ok {
			v.additional = &schemaRef{base: r.base, node: ap}
		}
		switch tt := n["type"].(type) {
		case string:
			v.types[tt] = true
		case []any:
			for _, x := range tt {
				v.types[fmt.Sprint(x)] = true
			}
		}
		if _, ok := n["const"]; ok {
			v.hasConst = true
		}
		if _, ok := n["enum"]; ok {
			v.hasEnum = true
		}
		if all, ok := n["allOf"].([]any); ok {
			for _, x := range all {
				if xm, ok := x.(map[string]any); ok {
					merge(schemaRef{base: r.base, node: xm})
				}
			}
		}
	}
	merge(ref)
	return v
}

func (w *driftWalker) resolveRef(base, ref string) (schemaRef, error) {
	file, frag, _ := strings.Cut(ref, "#")
	docURL := base
	if file != "" {
		dir := base[:strings.LastIndex(base, "/")+1]
		docURL = dir + file
	}
	doc, err := RawSchema(docURL)
	if err != nil {
		return schemaRef{}, err
	}
	node := any(doc)
	if frag != "" {
		for _, tok := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
			tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
			m, ok := node.(map[string]any)
			if !ok {
				return schemaRef{}, fmt.Errorf("pointer %s: not an object at %s", frag, tok)
			}
			node, ok = m[tok]
			if !ok {
				return schemaRef{}, fmt.Errorf("pointer %s: missing %s", frag, tok)
			}
		}
	}
	m, ok := node.(map[string]any)
	if !ok {
		return schemaRef{}, fmt.Errorf("ref %s: target is not an object", ref)
	}
	return schemaRef{base: docURL, node: m}, nil
}

// jsonFields returns json name -> field type, flattening embedded structs the
// way encoding/json does.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported
		}
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFields(ft) {
					out[k] = v
				}
				continue
			}
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
