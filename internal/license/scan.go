package license

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
)

// FileFunc receives one regular file from a Walker. path uses forward
// slashes and is relative to the tree root. r must not be used after
// FileFunc returns.
type FileFunc func(path string, size int64, r io.Reader) error

// Walker yields the regular files of a source tree.
type Walker interface {
	Walk(ctx context.Context, fn FileFunc) error
}

// Options tunes a scan.
type Options struct {
	// MaxLicenseBytes caps how much of a license file is read.
	MaxLicenseBytes int64
	// HeaderBytes is how much of each source file is checked for a header.
	HeaderBytes int
	// SkipDirs are directory names whose contents are ignored anywhere in
	// the tree: vendored dependencies and test fixtures.
	SkipDirs []string
}

// DefaultOptions are used when Scan is given a zero Options.
var DefaultOptions = Options{
	MaxLicenseBytes: 512 << 10,
	HeaderBytes:     4 << 10,
	SkipDirs:        []string{".git", "node_modules", "vendor", "third_party", "third-party", "testdata", "bower_components"},
}

// Report describes the licensing of one version of a source tree.
type Report struct {
	// Declared is the top-level license expression: from root license
	// files when present, else from root manifests. None if neither exists.
	Declared string `json:"declared"`
	// Licenses is every license ID declared at the top level, by root
	// license files or root manifests.
	Licenses []string `json:"licenses"`
	// Files lists every license file found, root and nested.
	Files []FileFinding `json:"files"`
	// Manifests lists root package manifests that declare a license.
	Manifests []ManifestFinding `json:"manifests"`
	Headers   HeaderSummary     `json:"headers"`
	Warnings  []string          `json:"warnings,omitempty"`
}

// FileFinding is one license file.
type FileFinding struct {
	Path string `json:"path"`
	// Root is true for license files at the top of the tree (or in a
	// top-level LICENSES/ directory).
	Root       bool    `json:"root"`
	Expression string  `json:"expression"`
	Coverage   float64 `json:"coverage"`
	SHA256     string  `json:"sha256"`
}

// ManifestFinding is a license declared in a package manifest.
type ManifestFinding struct {
	Path       string `json:"path"`
	Raw        string `json:"raw"`
	Expression string `json:"expression"` // NoAssertion if Raw didn't parse
}

// HeaderSummary aggregates per-file license headers.
type HeaderSummary struct {
	// Scanned is the number of source files checked.
	Scanned int `json:"scanned"`
	// ByLicense counts files per license ID.
	ByLicense map[string]int `json:"by_license"`
	// Files maps a file path to its header expression, for files with one.
	Files map[string]string `json:"files"`
}

// Marshal encodes the report as JSON.
func (r *Report) Marshal() ([]byte, error) { return json.Marshal(r) }

// UnmarshalReport decodes a report produced by Marshal.
func UnmarshalReport(b []byte) (*Report, error) {
	var r Report
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Scan walks a tree and builds its license report. It only reads files; it
// never runs anything from the tree.
func Scan(ctx context.Context, w Walker, opt Options) (*Report, error) {
	if opt.MaxLicenseBytes == 0 && opt.HeaderBytes == 0 && opt.SkipDirs == nil {
		opt = DefaultOptions
	}
	r := &Report{Headers: HeaderSummary{ByLicense: map[string]int{}, Files: map[string]string{}}}
	skip := make(map[string]bool, len(opt.SkipDirs))
	for _, d := range opt.SkipDirs {
		skip[strings.ToLower(d)] = true
	}

	err := w.Walk(ctx, func(p string, size int64, body io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir, base := path.Split(p)
		for _, part := range strings.Split(strings.Trim(dir, "/"), "/") {
			if skip[strings.ToLower(part)] {
				return nil
			}
		}
		switch {
		case isLicenseFile(dir, base):
			return r.addLicenseFile(p, dir, body, opt.MaxLicenseBytes)
		case dir == "" && manifestParsers[base] != nil:
			return r.addManifest(p, base, body)
		case isSourceFile(base):
			return r.addHeader(p, body, opt.HeaderBytes)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	r.finish()
	return r, nil
}

func (r *Report) addLicenseFile(p, dir string, body io.Reader, max int64) error {
	text, err := io.ReadAll(io.LimitReader(body, max))
	if err != nil {
		return fmt.Errorf("read %s: %w", p, err)
	}
	sum := sha256.Sum256(text)
	f := FileFinding{
		Path:   p,
		Root:   dir == "" || strings.EqualFold(dir, "LICENSES/"),
		SHA256: hex.EncodeToString(sum[:]),
	}
	c := ClassifyText(text)
	f.Coverage = round1(c.Coverage)
	if c.Expr != nil {
		f.Expression = c.Expr.String()
	} else {
		f.Expression = NoAssertion
	}
	if c.Expr != nil && c.Coverage < MinCoverage && !c.ByPhrase {
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s matches %s but only %.0f%% of its text is a known license; it may contain extra terms",
			p, f.Expression, c.Coverage))
	}
	r.Files = append(r.Files, f)
	return nil
}

func (r *Report) addManifest(p, base string, body io.Reader) error {
	data, err := io.ReadAll(io.LimitReader(body, 1<<20))
	if err != nil {
		return fmt.Errorf("read %s: %w", p, err)
	}
	raw := manifestParsers[base](data)
	if raw == "" {
		return nil
	}
	m := ManifestFinding{Path: p, Raw: raw}
	if strings.HasPrefix(strings.ToUpper(raw), "SEE LICENSE IN") {
		return nil // npm convention: defer to the referenced file
	}
	if e, err := ParseDeclared(raw); err == nil {
		m.Expression = e.String()
	} else {
		m.Expression = NoAssertion
		r.Warnings = append(r.Warnings, fmt.Sprintf("%s declares license %q, which is not a valid SPDX expression", p, raw))
	}
	r.Manifests = append(r.Manifests, m)
	return nil
}

func (r *Report) addHeader(p string, body io.Reader, n int) error {
	head := make([]byte, n)
	k, err := io.ReadFull(body, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return fmt.Errorf("read %s: %w", p, err)
	}
	head = head[:k]
	if bytes.IndexByte(head, 0) >= 0 {
		return nil // binary
	}
	r.Headers.Scanned++
	e := ClassifyHeader(head)
	if e == nil {
		return nil
	}
	r.Headers.Files[p] = e.String()
	for _, id := range e.Licenses() {
		r.Headers.ByLicense[id]++
	}
	return nil
}

// finish derives Declared and Licenses and sorts the findings.
func (r *Report) finish() {
	slices.SortFunc(r.Files, func(a, b FileFinding) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(r.Manifests, func(a, b ManifestFinding) int { return strings.Compare(a.Path, b.Path) })

	var fromFiles, fromManifests []*Expr
	ids := map[string]bool{}
	for _, f := range r.Files {
		if !f.Root {
			continue
		}
		e := mustParse(f.Expression)
		fromFiles = append(fromFiles, e)
		for _, id := range e.Licenses() {
			ids[id] = true
		}
	}
	manifestIDs := map[string]bool{}
	for _, m := range r.Manifests {
		e := mustParse(m.Expression)
		fromManifests = append(fromManifests, e)
		for _, id := range e.Licenses() {
			ids[id] = true
			manifestIDs[id] = true
		}
	}

	// License files are the legal text, so they win; manifests often lag
	// behind a relicense. Multiple root files are treated as all applying.
	switch {
	case len(fromFiles) > 0:
		r.Declared = Combine("AND", fromFiles...).String()
	case len(fromManifests) > 0:
		r.Declared = Combine("AND", fromManifests...).String()
	default:
		r.Declared = None
		ids[None] = true
	}
	r.Licenses = slices.Sorted(maps.Keys(ids))

	if len(fromFiles) > 0 && len(manifestIDs) > 0 {
		fileIDs := mustParse(r.Declared).Licenses()
		for _, m := range r.Manifests {
			mIDs := mustParse(m.Expression).Licenses()
			if !slices.Equal(mIDs, fileIDs) && m.Expression != NoAssertion {
				r.Warnings = append(r.Warnings, fmt.Sprintf("%s declares %s but the license files say %s",
					m.Path, m.Expression, r.Declared))
			}
		}
	}
}

// mustParse parses an expression this package produced itself.
func mustParse(s string) *Expr {
	e, err := Parse(s)
	if err != nil {
		return Leaf(NoAssertion)
	}
	return e
}

var licenseName = regexp.MustCompile(`(?i)^(licen[cs]e|copying|unlicense)([-._].*)?$`)

// codeExts are extensions that make a LICENSE-ish name a source file
// (license.go, License.java) rather than a license text.
var codeExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".ts": true, ".rs": true, ".java": true, ".kt": true,
	".c": true, ".h": true, ".cc": true, ".cpp": true, ".rb": true, ".php": true, ".cs": true,
	".json": true, ".yaml": true, ".yml": true, ".toml": true, ".xml": true, ".sh": true,
	".tsx": true, ".jsx": true, ".swift": true, ".scala": true, ".tmpl": true, ".tpl": true,
}

func isLicenseFile(dir, base string) bool {
	if strings.EqualFold(dir, "LICENSES/") {
		return !codeExts[strings.ToLower(path.Ext(base))]
	}
	return licenseName.MatchString(base) && !codeExts[strings.ToLower(path.Ext(base))]
}

var sourceExts = map[string]bool{
	".go": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".cxx": true, ".hpp": true,
	".java": true, ".kt": true, ".kts": true, ".scala": true, ".groovy": true, ".js": true,
	".jsx": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true, ".vue": true,
	".svelte": true, ".py": true, ".rb": true, ".rs": true, ".php": true, ".cs": true,
	".swift": true, ".m": true, ".mm": true, ".sh": true, ".bash": true, ".pl": true, ".pm": true,
	".lua": true, ".ex": true, ".exs": true, ".erl": true, ".hs": true, ".clj": true, ".dart": true,
	".sql": true, ".proto": true, ".css": true, ".scss": true, ".tf": true, ".zig": true,
	".nim": true, ".r": true, ".jl": true, ".ml": true, ".fs": true, ".vb": true, ".ps1": true,
}

func isSourceFile(base string) bool { return sourceExts[strings.ToLower(path.Ext(base))] }

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
