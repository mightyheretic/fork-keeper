package license

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// maxHeaderSamples caps how many changed headers a Diff lists.
const maxHeaderSamples = 20

// coverageDrop is how far a license file's match coverage must fall, in
// percentage points, to count as a change when the IDs stay the same, for
// example extra terms appended to an MIT license.
const coverageDrop = 5

// Diff describes license changes between two versions.
type Diff struct {
	OldRef string `json:"old_ref"`
	NewRef string `json:"new_ref"`
	// Changed is true when anything license-relevant changed.
	Changed     bool   `json:"changed"`
	OldDeclared string `json:"old_declared"`
	NewDeclared string `json:"new_declared"`
	// OldLicenses and NewLicenses are the full top-level license sets.
	OldLicenses []string `json:"old_licenses"`
	NewLicenses []string `json:"new_licenses"`
	// Added and Removed are top-level license IDs.
	Added   []string     `json:"added,omitempty"`
	Removed []string     `json:"removed,omitempty"`
	Files   []FileChange `json:"files,omitempty"`
	Headers HeaderDiff   `json:"headers"`
	// NewWarnings are warnings in the new report that the old one didn't have.
	NewWarnings []string `json:"new_warnings,omitempty"`
}

// FileChange is a license file that appeared, disappeared or changed.
type FileChange struct {
	Path        string  `json:"path"`
	Change      string  `json:"change"` // added | removed | relicensed | text-changed
	Root        bool    `json:"root"`
	Old         string  `json:"old,omitempty"`
	New         string  `json:"new,omitempty"`
	OldCoverage float64 `json:"old_coverage,omitempty"`
	NewCoverage float64 `json:"new_coverage,omitempty"`
}

// HeaderDiff summarizes per-file header changes.
type HeaderDiff struct {
	// Added and Removed are license IDs that appear in (or vanish from)
	// file headers across the tree.
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	// Relicensed counts files present in both versions whose header changed.
	Relicensed int `json:"relicensed"`
	// Samples lists up to 20 of those files.
	Samples   []HeaderChange `json:"samples,omitempty"`
	OldCounts map[string]int `json:"old_counts,omitempty"`
	NewCounts map[string]int `json:"new_counts,omitempty"`
}

// HeaderChange is one file whose header license changed.
type HeaderChange struct {
	Path string `json:"path"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// Compare diffs two reports.
func Compare(oldRef string, old *Report, newRef string, cur *Report) *Diff {
	d := &Diff{
		OldRef: oldRef, NewRef: newRef,
		OldDeclared: old.Declared, NewDeclared: cur.Declared,
		OldLicenses: old.Licenses, NewLicenses: cur.Licenses,
	}
	d.Added, d.Removed = setDiff(old.Licenses, cur.Licenses)

	oldFiles := map[string]FileFinding{}
	for _, f := range old.Files {
		oldFiles[f.Path] = f
	}
	for _, f := range cur.Files {
		o, ok := oldFiles[f.Path]
		delete(oldFiles, f.Path)
		switch {
		case !ok:
			d.Files = append(d.Files, FileChange{Path: f.Path, Change: "added", Root: f.Root, New: f.Expression, NewCoverage: f.Coverage})
		case o.Expression != f.Expression:
			d.Files = append(d.Files, FileChange{Path: f.Path, Change: "relicensed", Root: f.Root,
				Old: o.Expression, New: f.Expression, OldCoverage: o.Coverage, NewCoverage: f.Coverage})
		case o.SHA256 != f.SHA256 && o.Coverage-f.Coverage >= coverageDrop:
			d.Files = append(d.Files, FileChange{Path: f.Path, Change: "text-changed", Root: f.Root,
				Old: o.Expression, New: f.Expression, OldCoverage: o.Coverage, NewCoverage: f.Coverage})
		}
	}
	for _, p := range slices.Sorted(maps.Keys(oldFiles)) {
		o := oldFiles[p]
		d.Files = append(d.Files, FileChange{Path: p, Change: "removed", Root: o.Root, Old: o.Expression, OldCoverage: o.Coverage})
	}
	slices.SortFunc(d.Files, func(a, b FileChange) int { return strings.Compare(a.Path, b.Path) })

	d.Headers.Added, d.Headers.Removed = setDiff(slices.Sorted(maps.Keys(old.Headers.ByLicense)), slices.Sorted(maps.Keys(cur.Headers.ByLicense)))
	for _, p := range slices.Sorted(maps.Keys(cur.Headers.Files)) {
		n := cur.Headers.Files[p]
		o, ok := old.Headers.Files[p]
		if !ok || o == n {
			continue
		}
		d.Headers.Relicensed++
		if len(d.Headers.Samples) < maxHeaderSamples {
			d.Headers.Samples = append(d.Headers.Samples, HeaderChange{Path: p, Old: o, New: n})
		}
	}
	if len(d.Headers.Added)+len(d.Headers.Removed) > 0 || d.Headers.Relicensed > 0 {
		d.Headers.OldCounts = old.Headers.ByLicense
		d.Headers.NewCounts = cur.Headers.ByLicense
	}

	oldWarn := map[string]bool{}
	for _, w := range old.Warnings {
		oldWarn[w] = true
	}
	for _, w := range cur.Warnings {
		if !oldWarn[w] {
			d.NewWarnings = append(d.NewWarnings, w)
		}
	}

	d.Changed = d.OldDeclared != d.NewDeclared || len(d.Added) > 0 || len(d.Removed) > 0 ||
		len(d.Files) > 0 || len(d.Headers.Added) > 0 || len(d.Headers.Removed) > 0 || d.Headers.Relicensed > 0
	return d
}

// setDiff returns the elements only in b (added) and only in a (removed).
func setDiff(a, b []string) (added, removed []string) {
	for _, x := range b {
		if !slices.Contains(a, x) {
			added = append(added, x)
		}
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			removed = append(removed, x)
		}
	}
	return added, removed
}

// Summary is a one-line description of the diff.
func (d *Diff) Summary() string {
	if !d.Changed {
		return fmt.Sprintf("no license change between %s and %s (%s)", d.OldRef, d.NewRef, d.NewDeclared)
	}
	var parts []string
	if d.OldDeclared != d.NewDeclared {
		parts = append(parts, fmt.Sprintf("declared %s -> %s", d.OldDeclared, d.NewDeclared))
	}
	if len(d.Added) > 0 {
		parts = append(parts, "added "+strings.Join(d.Added, ", "))
	}
	if len(d.Removed) > 0 {
		parts = append(parts, "removed "+strings.Join(d.Removed, ", "))
	}
	if n := len(d.Files); n > 0 {
		parts = append(parts, fmt.Sprintf("%d license file change(s)", n))
	}
	if len(d.Headers.Added) > 0 {
		parts = append(parts, "new in headers: "+strings.Join(d.Headers.Added, ", "))
	}
	if d.Headers.Relicensed > 0 {
		parts = append(parts, fmt.Sprintf("%d file header(s) relicensed", d.Headers.Relicensed))
	}
	return fmt.Sprintf("license change between %s and %s: %s", d.OldRef, d.NewRef, strings.Join(parts, "; "))
}

// WriteText prints a human-readable report.
func (r *Report) WriteText(w io.Writer) {
	fmt.Fprintf(w, "Declared: %s\n", r.Declared)
	fmt.Fprintf(w, "Top-level licenses: %s\n", strings.Join(r.Licenses, ", "))
	if len(r.Files) > 0 {
		fmt.Fprintln(w, "\nLicense files:")
		for _, f := range r.Files {
			where := "nested"
			if f.Root {
				where = "root"
			}
			fmt.Fprintf(w, "  %-40s %-6s %s (%.0f%% matched)\n", f.Path, where, f.Expression, f.Coverage)
		}
	}
	if len(r.Manifests) > 0 {
		fmt.Fprintln(w, "\nManifests:")
		for _, m := range r.Manifests {
			fmt.Fprintf(w, "  %-40s %s\n", m.Path, m.Expression)
		}
	}
	fmt.Fprintf(w, "\nFile headers: %d with a license notice, of %d source files scanned\n", len(r.Headers.Files), r.Headers.Scanned)
	for _, id := range slices.Sorted(maps.Keys(r.Headers.ByLicense)) {
		fmt.Fprintf(w, "  %-30s %d\n", id, r.Headers.ByLicense[id])
	}
	writeList(w, "\nWarnings:", r.Warnings)
}

// WriteText prints a human-readable diff.
func (d *Diff) WriteText(w io.Writer) {
	fmt.Fprintln(w, d.Summary())
	fmt.Fprintf(w, "\nDeclared: %s -> %s\n", d.OldDeclared, d.NewDeclared)
	if len(d.Added)+len(d.Removed) > 0 {
		fmt.Fprintf(w, "Top-level licenses added: %s; removed: %s\n", orNone(d.Added), orNone(d.Removed))
	}
	if len(d.Files) > 0 {
		fmt.Fprintln(w, "\nLicense files:")
		for _, f := range d.Files {
			switch f.Change {
			case "added":
				fmt.Fprintf(w, "  + %s: %s\n", f.Path, f.New)
			case "removed":
				fmt.Fprintf(w, "  - %s: %s\n", f.Path, f.Old)
			case "relicensed":
				fmt.Fprintf(w, "  ~ %s: %s -> %s\n", f.Path, f.Old, f.New)
			default:
				fmt.Fprintf(w, "  ~ %s: text changed, %.0f%% -> %.0f%% recognized\n", f.Path, f.OldCoverage, f.NewCoverage)
			}
		}
	}
	h := d.Headers
	if len(h.Added)+len(h.Removed) > 0 || h.Relicensed > 0 {
		fmt.Fprintf(w, "\nFile headers: added %s; removed %s; %d file(s) relicensed\n", orNone(h.Added), orNone(h.Removed), h.Relicensed)
		ids := map[string]bool{}
		for id := range h.OldCounts {
			ids[id] = true
		}
		for id := range h.NewCounts {
			ids[id] = true
		}
		for _, id := range slices.Sorted(maps.Keys(ids)) {
			fmt.Fprintf(w, "  %-30s %6d -> %d\n", id, h.OldCounts[id], h.NewCounts[id])
		}
		for _, s := range h.Samples {
			fmt.Fprintf(w, "  ~ %s: %s -> %s\n", s.Path, s.Old, s.New)
		}
		if h.Relicensed > len(h.Samples) {
			fmt.Fprintf(w, "  ... and %d more\n", h.Relicensed-len(h.Samples))
		}
	}
	writeList(w, "\nNew warnings:", d.NewWarnings)
}

func writeList(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(w, title)
	for _, s := range items {
		fmt.Fprintln(w, "  "+s)
	}
}

func orNone(s []string) string {
	if len(s) == 0 {
		return "none"
	}
	return strings.Join(s, ", ")
}

// Marshal encodes the diff as JSON.
func (d *Diff) Marshal() ([]byte, error) { return json.Marshal(d) }
