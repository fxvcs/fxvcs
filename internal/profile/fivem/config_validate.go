package fivem

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

const maxGeneratedCfgLine = 1 << 20

var convarNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

var directCfgDirectives = map[string]bool{
	"sv_licensekey":  true,
	"rcon_password":  true,
	"sv_hostname":    true,
	"sv_projectname": true,
	"sv_projectdesc": true,
}

var txAdminOwnedConvars = map[string]bool{
	"sv_appearallowlisted":     true,
	"sv_allowlistinstructions": true,
	"txadmin-menuenabled":      true,
	"txadmin-luacomhost":       true,
	"txadmin-luacomtoken":      true,
	"txadminservermode":        true,
	"txadmin-version":          true,
}

var fxvcsOwnedConvars = map[string]bool{
	"fxvcs_health_token": true,
}

// ValidateConfigFragment accepts the deliberately small v1 generated-cfg
// grammar. Resource lifecycle and exec lines are owned by RuntimeConfig, while
// operator/txAdmin startup directives remain outside fxvcs-generated output.
func ValidateConfigFragment(data []byte) error {
	if bytes.IndexByte(data, 0) >= 0 {
		return fmt.Errorf("%w: NUL byte", ErrRuntimeConfig)
	}
	seenConvars := map[string]int{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), maxGeneratedCfgLine)
	line := 0
	for scanner.Scan() {
		line++
		tokens, err := runtimeCfgTokens(scanner.Text())
		if err != nil {
			return fmt.Errorf("%w: line %d: %v", ErrRuntimeConfig, line, err)
		}
		if len(tokens) == 0 {
			continue
		}
		directive := strings.ToLower(tokens[0])
		var convar string
		switch directive {
		case "set", "setr", "sets":
			if len(tokens) != 3 {
				return fmt.Errorf("%w: line %d: %s requires one name and one cfg-quoted value", ErrRuntimeConfig, line, tokens[0])
			}
			convar = tokens[1]
		case "+set":
			return fmt.Errorf("%w: line %d: +set is a process argument, not a cfg directive", ErrRuntimeConfig, line)
		default:
			if !directCfgDirectives[directive] {
				return fmt.Errorf("%w: line %d: directive %q is not owned by fxvcs", ErrRuntimeConfig, line, tokens[0])
			}
			if len(tokens) != 2 {
				return fmt.Errorf("%w: line %d: %s requires one cfg-quoted value", ErrRuntimeConfig, line, tokens[0])
			}
			convar = tokens[0]
		}
		if !convarNameRE.MatchString(convar) {
			return fmt.Errorf("%w: line %d: invalid convar name %q", ErrRuntimeConfig, line, convar)
		}
		key := strings.ToLower(convar)
		if strings.HasPrefix(key, "txadmin-") || txAdminOwnedConvars[key] {
			return fmt.Errorf("%w: line %d: convar %q is owned by txAdmin", ErrRuntimeConfig, line, convar)
		}
		if fxvcsOwnedConvars[key] {
			return fmt.Errorf("%w: line %d: convar %q is owned by fxvcs", ErrRuntimeConfig, line, convar)
		}
		if previous, exists := seenConvars[key]; exists {
			return fmt.Errorf("%w: line %d: convar %q duplicates line %d", ErrRuntimeConfig, line, convar, previous)
		}
		seenConvars[key] = line
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrRuntimeConfig, err)
	}
	return nil
}

func runtimeCfgTokens(line string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	inToken := false
	var quote rune
	escaped := false
	flush := func() {
		if inToken {
			tokens = append(tokens, current.String())
			current.Reset()
			inToken = false
		}
	}
	for _, char := range line {
		if quote != 0 {
			if escaped {
				current.WriteRune(char)
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == quote {
				quote = 0
				continue
			}
			current.WriteRune(char)
			continue
		}
		switch char {
		case '#':
			flush()
			return tokens, nil
		case '\'', '"':
			quote = char
			inToken = true
		case ' ', '\t', '\r':
			flush()
		default:
			inToken = true
			current.WriteRune(char)
		}
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated quoted value")
	}
	flush()
	return tokens, nil
}
