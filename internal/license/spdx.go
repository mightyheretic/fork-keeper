// Package license detects which licenses a source tree is under and what
// changed between two versions: SPDX identifiers in package manifests,
// LICENSE/COPYING files, and per-file license headers.
package license

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Special identifiers, following SPDX usage.
const (
	// None means no license was found at all.
	None = "NONE"
	// NoAssertion means a license is present but could not be identified.
	NoAssertion = "NOASSERTION"
)

// Expr is a parsed SPDX license expression.
type Expr struct {
	// Op is "AND" or "OR" for compound expressions, "" for a single license.
	Op   string
	Args []*Expr
	// ID and Exception are set when Op is "".
	ID        string
	Exception string
}

// Licenses returns the license IDs in e, sorted and de-duplicated.
func (e *Expr) Licenses() []string {
	var ids []string
	var walk func(*Expr)
	walk = func(e *Expr) {
		if e.Op == "" {
			ids = append(ids, e.ID)
			return
		}
		for _, a := range e.Args {
			walk(a)
		}
	}
	walk(e)
	slices.Sort(ids)
	return slices.Compact(ids)
}

// String returns the canonical form: nested operators flattened, operands
// sorted, and parentheses only where needed. Equivalent expressions such as
// "MIT OR Apache-2.0" and "(Apache-2.0 OR MIT)" print identically.
func (e *Expr) String() string { return e.format(false) }

func (e *Expr) format(nested bool) string {
	if e.Op == "" {
		if e.Exception != "" {
			return e.ID + " WITH " + e.Exception
		}
		return e.ID
	}
	parts := make([]string, len(e.Args))
	for i, a := range e.Args {
		parts[i] = a.format(true)
	}
	s := strings.Join(parts, " "+e.Op+" ")
	if nested {
		return "(" + s + ")"
	}
	return s
}

// Combine joins expressions with op ("AND" or "OR") in canonical form.
// It returns nil when exprs is empty.
func Combine(op string, exprs ...*Expr) *Expr {
	var args []*Expr
	for _, e := range exprs {
		if e == nil {
			continue
		}
		if e.Op == op {
			args = append(args, e.Args...)
		} else {
			args = append(args, e)
		}
	}
	slices.SortFunc(args, func(a, b *Expr) int { return strings.Compare(a.format(true), b.format(true)) })
	args = slices.CompactFunc(args, func(a, b *Expr) bool { return a.format(true) == b.format(true) })
	switch len(args) {
	case 0:
		return nil
	case 1:
		return args[0]
	}
	return &Expr{Op: op, Args: args}
}

// Leaf returns a single-license expression for id, normalized.
func Leaf(id string) *Expr { return &Expr{ID: Normalize(id)} }

// ParseDeclared parses a license string as written in a package manifest.
// It accepts SPDX expressions plus common informal names ("Apache 2.0",
// "GPLv3", "UNLICENSED").
func ParseDeclared(s string) (*Expr, error) {
	s = strings.TrimSpace(s)
	if id, ok := aliases[strings.ToLower(s)]; ok {
		return &Expr{ID: id}, nil
	}
	return Parse(s)
}

// Parse parses an SPDX license expression. Operators are accepted in any
// case, and IDs are normalized to their current SPDX spelling.
func Parse(s string) (*Expr, error) {
	p := &parser{tokens: tokenize(s)}
	if len(p.tokens) == 0 {
		return nil, fmt.Errorf("empty license expression")
	}
	e, err := p.or()
	if err != nil {
		return nil, fmt.Errorf("license expression %q: %w", s, err)
	}
	if p.pos != len(p.tokens) {
		return nil, fmt.Errorf("license expression %q: unexpected %q", s, p.tokens[p.pos])
	}
	return e, nil
}

func tokenize(s string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '(' || r == ')':
			flush()
			tokens = append(tokens, string(r))
		case unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return tokens
}

type parser struct {
	tokens []string
	pos    int
}

func (p *parser) peekOp(op string) bool {
	return p.pos < len(p.tokens) && strings.EqualFold(p.tokens[p.pos], op)
}

func (p *parser) or() (*Expr, error) {
	return p.binary("OR", p.and)
}

func (p *parser) and() (*Expr, error) {
	return p.binary("AND", p.with)
}

func (p *parser) binary(op string, next func() (*Expr, error)) (*Expr, error) {
	first, err := next()
	if err != nil {
		return nil, err
	}
	args := []*Expr{first}
	for p.peekOp(op) {
		p.pos++
		e, err := next()
		if err != nil {
			return nil, err
		}
		args = append(args, e)
	}
	return Combine(op, args...), nil
}

func (p *parser) with() (*Expr, error) {
	e, err := p.atom()
	if err != nil {
		return nil, err
	}
	if p.peekOp("WITH") {
		p.pos++
		if p.pos >= len(p.tokens) || !isIDToken(p.tokens[p.pos]) {
			return nil, fmt.Errorf("WITH needs an exception identifier")
		}
		if e.Op != "" {
			return nil, fmt.Errorf("WITH must follow a single license")
		}
		e.Exception = p.tokens[p.pos]
		p.pos++
	}
	return e, nil
}

func (p *parser) atom() (*Expr, error) {
	if p.pos >= len(p.tokens) {
		return nil, fmt.Errorf("unexpected end of expression")
	}
	tok := p.tokens[p.pos]
	p.pos++
	if tok == "(" {
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.pos >= len(p.tokens) || p.tokens[p.pos] != ")" {
			return nil, fmt.Errorf("missing closing parenthesis")
		}
		p.pos++
		return e, nil
	}
	if !isIDToken(tok) {
		return nil, fmt.Errorf("unexpected %q", tok)
	}
	return Leaf(tok), nil
}

func isIDToken(tok string) bool {
	if tok == "(" || tok == ")" {
		return false
	}
	for _, op := range []string{"AND", "OR", "WITH"} {
		if strings.EqualFold(tok, op) {
			return false
		}
	}
	for _, r := range tok {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '+' || r == ':') {
			return false
		}
	}
	return true
}

// Normalize returns the current SPDX spelling of id: canonical case,
// deprecated GPL-family IDs mapped to -only/-or-later, and known aliases.
// Unknown IDs are returned unchanged.
func Normalize(id string) string {
	lower := strings.ToLower(id)
	if strings.HasSuffix(lower, "+") {
		base := Normalize(strings.TrimSuffix(id, "+"))
		if fam, ok := strings.CutSuffix(base, "-only"); ok {
			return fam + "-or-later"
		}
		return base + "-or-later"
	}
	if canon, ok := deprecated[lower]; ok {
		return canon
	}
	if canon, ok := aliases[lower]; ok {
		return canon
	}
	if canon, ok := knownIDs[lower]; ok {
		return canon
	}
	if rest, ok := strings.CutPrefix(lower, "licenseref-"); ok && rest != "" {
		return "LicenseRef-" + id[len("licenseref-"):]
	}
	return id
}

// Known reports whether id (after Normalize) is a recognized license.
func Known(id string) bool {
	_, ok := knownIDs[strings.ToLower(id)]
	return ok || strings.HasPrefix(id, "LicenseRef-") || id == None || id == NoAssertion
}

// deprecated maps old SPDX IDs (and licensecheck's IDs) to current ones.
var deprecated = map[string]string{}

// aliases maps informal names seen in manifests to SPDX IDs.
var aliases = map[string]string{
	"apache 2":                    "Apache-2.0",
	"apache 2.0":                  "Apache-2.0",
	"apache-2":                    "Apache-2.0",
	"apache2":                     "Apache-2.0",
	"apache license 2.0":          "Apache-2.0",
	"apache license, version 2.0": "Apache-2.0",
	"apache software license":     "Apache-2.0",
	"mit license":                 "MIT",
	"the mit license":             "MIT",
	"expat":                       "MIT",
	"bsd":                         "LicenseRef-BSD",
	"new bsd":                     "BSD-3-Clause",
	"bsd-3":                       "BSD-3-Clause",
	"bsd 3-clause":                "BSD-3-Clause",
	"bsd-2":                       "BSD-2-Clause",
	"simplified bsd":              "BSD-2-Clause",
	"isc license":                 "ISC",
	"mpl 2.0":                     "MPL-2.0",
	"mpl-2":                       "MPL-2.0",
	"gplv2":                       "GPL-2.0-only",
	"gplv2+":                      "GPL-2.0-or-later",
	"gplv3":                       "GPL-3.0-only",
	"gplv3+":                      "GPL-3.0-or-later",
	"lgplv2.1":                    "LGPL-2.1-only",
	"lgplv3":                      "LGPL-3.0-only",
	"agplv3":                      "AGPL-3.0-only",
	"public domain":               "LicenseRef-Public-Domain",
	"unlicensed":                  "LicenseRef-UNLICENSED", // npm: proprietary, not The Unlicense
	"proprietary":                 "LicenseRef-Proprietary",
	"bsl-1.1":                     "BUSL-1.1", // a common mistake: BSL-1.0 is the Boost license
	"bsl 1.1":                     "BUSL-1.1",
	"business source license 1.1": "BUSL-1.1",
	"sspl":                        "SSPL-1.0",
	"sspl-1":                      "SSPL-1.0",
	"server side public license":  "SSPL-1.0",
	"elastic license 2.0":         "Elastic-2.0",
	"elv2":                        "Elastic-2.0",
	"elastic-2":                   "Elastic-2.0",
	"rsalv2":                      "LicenseRef-RSAL-2.0",
	"commons clause":              "LicenseRef-Commons-Clause",
	"commonsclause":               "LicenseRef-Commons-Clause", // licensecheck's ID
}

// knownIDs holds canonical SPDX IDs, keyed by lower case.
var knownIDs = map[string]string{}

func init() {
	for _, id := range []string{
		"0BSD", "AFL-3.0", "AGPL-1.0-only", "AGPL-1.0-or-later", "AGPL-3.0-only", "AGPL-3.0-or-later",
		"Apache-1.0", "Apache-1.1", "Apache-2.0", "APSL-2.0", "Artistic-1.0", "Artistic-2.0",
		"BlueOak-1.0.0", "BSD-1-Clause", "BSD-2-Clause", "BSD-2-Clause-Patent", "BSD-3-Clause",
		"BSD-3-Clause-Clear", "BSD-4-Clause", "BSL-1.0", "BUSL-1.1", "CC-BY-4.0", "CC-BY-SA-4.0",
		"CC0-1.0", "CDDL-1.0", "CDDL-1.1", "CPAL-1.0", "curl", "ECL-2.0", "Elastic-2.0", "EPL-1.0",
		"EPL-2.0", "EUPL-1.1", "EUPL-1.2", "FSL-1.1-ALv2", "FSL-1.1-MIT", "GPL-1.0-only",
		"GPL-1.0-or-later", "GPL-2.0-only", "GPL-2.0-or-later", "GPL-3.0-only", "GPL-3.0-or-later",
		"ISC", "LGPL-2.0-only", "LGPL-2.0-or-later", "LGPL-2.1-only", "LGPL-2.1-or-later",
		"LGPL-3.0-only", "LGPL-3.0-or-later", "LPPL-1.3c", "MIT", "MIT-0", "MPL-1.1", "MPL-2.0",
		"MPL-2.0-no-copyleft-exception", "MS-PL", "MS-RL", "MulanPSL-2.0", "NCSA", "ODbL-1.0",
		"OFL-1.1", "OpenSSL", "OSL-3.0", "PHP-3.01", "PolyForm-Noncommercial-1.0.0",
		"PolyForm-Small-Business-1.0.0", "PostgreSQL", "PSF-2.0", "Python-2.0", "Ruby", "SSPL-1.0",
		"Unicode-3.0", "Unicode-DFS-2016", "Unlicense", "UPL-1.0", "Vim", "W3C", "WTFPL", "X11",
		"Zlib", "zlib-acknowledgement", "ZPL-2.1",
	} {
		knownIDs[strings.ToLower(id)] = id
	}
	for _, fam := range []string{"GPL-1.0", "GPL-2.0", "GPL-3.0", "LGPL-2.0", "LGPL-2.1", "LGPL-3.0", "AGPL-1.0", "AGPL-3.0"} {
		l := strings.ToLower(fam)
		deprecated[l] = fam + "-only"
		deprecated[l+"+"] = fam + "-or-later"
	}
	// An unversioned GPL lets the recipient pick any version.
	deprecated["gpl"] = "GPL-1.0-or-later"
}
