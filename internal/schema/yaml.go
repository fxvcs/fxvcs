package schema

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ParseYAML parses one YAML (or JSON) document into the generic JSON-compatible
// form used by the validators: map[string]any, []any, string, int64, float64,
// bool, nil. It rejects what canonical JSON cannot represent or what makes a
// document ambiguous:
//
//   - more than one document in the stream;
//   - non-string mapping keys;
//   - duplicate mapping keys;
//   - integers outside int64;
//   - unresolvable aliases or unknown tags.
//
// Timestamps stay strings (their raw scalar text) so RFC 3339 values are
// validated by pattern rather than re-formatted.
func ParseYAML(data []byte) (any, Issues) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if err.Error() == "EOF" {
			return nil, Issues{{Message: "empty document"}}
		}
		return nil, Issues{{Message: "yaml: " + strings.TrimPrefix(err.Error(), "yaml: ")}}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, Issues{{Message: "yaml: stream contains more than one document"}}
	} else if err.Error() != "EOF" {
		return nil, Issues{{Message: "yaml: " + strings.TrimPrefix(err.Error(), "yaml: ")}}
	}
	c := &yamlConverter{}
	v, err := c.convert(&root, "")
	if err != nil {
		return nil, Issues{*err}
	}
	return v, nil
}

type yamlConverter struct {
	depth int
}

func (c *yamlConverter) convert(n *yaml.Node, path string) (any, *Issue) {
	c.depth++
	defer func() { c.depth-- }()
	if c.depth > 256 {
		return nil, &Issue{Path: path, Message: "yaml: nesting too deep"}
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return nil, &Issue{Message: "empty document"}
		}
		return c.convert(n.Content[0], path)
	case yaml.AliasNode:
		if n.Alias == nil {
			return nil, &Issue{Path: path, Message: "yaml: unresolved alias"}
		}
		return c.convert(n.Alias, path)
	case yaml.MappingNode:
		m := make(map[string]any, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			kn, vn := n.Content[i], n.Content[i+1]
			if kn.Kind == yaml.AliasNode && kn.Alias != nil {
				kn = kn.Alias
			}
			if kn.Kind != yaml.ScalarNode || (kn.Tag != "!!str" && kn.Tag != "") {
				return nil, &Issue{Path: path, Message: fmt.Sprintf("yaml: line %d: mapping keys must be strings", kn.Line)}
			}
			key := kn.Value
			if _, dup := m[key]; dup {
				return nil, &Issue{Path: path + "/" + escapePointer(key), Message: fmt.Sprintf("yaml: line %d: duplicate key %q", kn.Line, key)}
			}
			v, err := c.convert(vn, path+"/"+escapePointer(key))
			if err != nil {
				return nil, err
			}
			m[key] = v
		}
		return m, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for i, e := range n.Content {
			v, err := c.convert(e, fmt.Sprintf("%s/%d", path, i))
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		return c.scalar(n, path)
	}
	return nil, &Issue{Path: path, Message: "yaml: unsupported node"}
}

func (c *yamlConverter) scalar(n *yaml.Node, path string) (any, *Issue) {
	switch n.Tag {
	case "!!null":
		return nil, nil
	case "!!bool":
		switch strings.ToLower(n.Value) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		// YAML 1.1 spellings (yes/no/on/off) are strings, never booleans.
		return n.Value, nil
	case "!!int":
		s := n.Value
		neg := strings.HasPrefix(s, "-")
		u := strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")
		u = strings.ReplaceAll(u, "_", "")
		var v uint64
		var err error
		switch {
		case strings.HasPrefix(u, "0x"), strings.HasPrefix(u, "0X"):
			v, err = strconv.ParseUint(u[2:], 16, 64)
		case strings.HasPrefix(u, "0o"):
			v, err = strconv.ParseUint(u[2:], 8, 64)
		case strings.HasPrefix(u, "0b"):
			v, err = strconv.ParseUint(u[2:], 2, 64)
		default:
			v, err = strconv.ParseUint(u, 10, 64)
		}
		if err != nil || v > math.MaxInt64 {
			return nil, &Issue{Path: path, Message: fmt.Sprintf("yaml: line %d: integer %q out of range", n.Line, s)}
		}
		if neg {
			return -int64(v), nil
		}
		return int64(v), nil
	case "!!float":
		f, err := strconv.ParseFloat(strings.ReplaceAll(n.Value, "_", ""), 64)
		if err != nil {
			return nil, &Issue{Path: path, Message: fmt.Sprintf("yaml: line %d: invalid float %q", n.Line, n.Value)}
		}
		return f, nil
	case "!!str", "", "!!timestamp":
		return n.Value, nil
	case "!!binary", "!!merge", "!!set", "!!omap", "!!pairs", "!!seq", "!!map":
		return nil, &Issue{Path: path, Message: fmt.Sprintf("yaml: line %d: unsupported tag %s", n.Line, n.Tag)}
	}
	if strings.HasPrefix(n.Tag, "!") {
		return nil, &Issue{Path: path, Message: fmt.Sprintf("yaml: line %d: unsupported tag %s", n.Line, n.Tag)}
	}
	return n.Value, nil
}
