package license

import (
	"encoding/json"
	"regexp"
	"strings"
)

// manifestParsers extract the declared license string from root package
// manifests. Each returns "" when the manifest declares none.
var manifestParsers = map[string]func([]byte) string{
	"package.json":   jsonLicense,
	"composer.json":  jsonLicense,
	"Cargo.toml":     tomlLicense("package"),
	"pyproject.toml": tomlLicense("project", "tool.poetry"),
}

// jsonLicense reads "license" from package.json or composer.json. It also
// accepts the legacy {"type": ...} object and composer's array form, whose
// entries are alternatives.
func jsonLicense(data []byte) string {
	var m struct {
		License  json.RawMessage `json:"license"`
		Licenses []struct {
			Type string `json:"type"`
		} `json:"licenses"`
	}
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(m.License, &s) == nil {
		return s
	}
	var obj struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(m.License, &obj) == nil && obj.Type != "" {
		return obj.Type
	}
	var list []string
	if json.Unmarshal(m.License, &list) == nil && len(list) > 0 {
		return joinAlternatives(list)
	}
	if len(m.Licenses) > 0 {
		types := make([]string, 0, len(m.Licenses))
		for _, l := range m.Licenses {
			types = append(types, l.Type)
		}
		return joinAlternatives(types)
	}
	return ""
}

func joinAlternatives(list []string) string {
	if len(list) == 1 {
		return list[0]
	}
	parts := make([]string, len(list))
	for i, s := range list {
		parts[i] = "(" + s + ")"
	}
	return strings.Join(parts, " OR ")
}

var (
	tomlTable   = regexp.MustCompile(`^\s*\[([^\]]+)\]\s*$`)
	tomlLicStr  = regexp.MustCompile(`^\s*license\s*=\s*["']([^"']+)["']`)
	tomlLicText = regexp.MustCompile(`^\s*license\s*=\s*\{[^}]*\btext\s*=\s*["']([^"']+)["']`)
)

// tomlLicense reads `license = "..."` (or PEP 621's `{ text = "..." }`)
// from the first matching table. It's a line-based reader, not a full
// TOML parser, which is enough for these well-known keys.
func tomlLicense(tables ...string) func([]byte) string {
	return func(data []byte) string {
		current := ""
		for _, line := range strings.Split(string(data), "\n") {
			if m := tomlTable.FindStringSubmatch(line); m != nil {
				current = strings.TrimSpace(m[1])
				continue
			}
			want := false
			for _, t := range tables {
				if current == t {
					want = true
				}
			}
			if !want {
				continue
			}
			if m := tomlLicStr.FindStringSubmatch(line); m != nil {
				return m[1]
			}
			if m := tomlLicText.FindStringSubmatch(line); m != nil {
				return m[1]
			}
		}
		return ""
	}
}
