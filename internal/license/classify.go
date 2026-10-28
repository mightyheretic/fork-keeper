package license

import (
	"regexp"
	"slices"
	"strings"

	"github.com/google/licensecheck"
)

// MinCoverage is the share of a license file's text (in percent) that must
// match known licenses for the file to count as identified without a
// phrase match.
const MinCoverage = 75

// Classification is the result of identifying a license text.
type Classification struct {
	// Expr is the identified license expression, or nil if unknown.
	Expr *Expr
	// Coverage is the percentage of the text matched by licensecheck.
	Coverage float64
	// ByPhrase is true when a distinctive phrase identified a license that
	// licensecheck doesn't know, so Coverage says nothing about extra terms.
	ByPhrase bool
}

// phrase identifies a license by a distinctive phrase. These cover licenses
// licensecheck doesn't know, which are mostly the source-available
// licenses a relicensing tripwire most needs to catch.
type phrase struct {
	re *regexp.Regexp
	id string
}

func phrases(pairs ...string) []phrase {
	out := make([]phrase, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, phrase{re: regexp.MustCompile(pairs[i]), id: pairs[i+1]})
	}
	return out
}

// textPhrases apply to full license files (normalized, lower case).
var textPhrases = phrases(
	`business source license(?: version)? 1\.1`, "BUSL-1.1",
	`elastic license(?: version)? 2\.0|elastic license 2`, "Elastic-2.0",
	`server side public license`, "SSPL-1.0",
	`redis source available license(?: agreement)?,? (?:version 2|v ?2)`, "LicenseRef-RSAL-2.0",
	`functional source license.{0,200}mit future license`, "FSL-1.1-MIT",
	`functional source license.{0,200}apache (?:license )?2\.0 future license`, "FSL-1.1-ALv2",
	`"commons clause" license condition`, "LicenseRef-Commons-Clause",
	`confluent community license`, "LicenseRef-Confluent-Community-1.0",
	`sustainable use license`, "LicenseRef-Sustainable-Use-1.0",
	`fair source license`, "LicenseRef-Fair-Source",
	`gnu affero general public license,? version 3`, "AGPL-3.0-only",
	`^elastic license agreement`, "LicenseRef-Elastic-1.0",
)

// headerPhrases apply to the first few KB of source files. They match the
// standard notices projects put in file headers.
var headerPhrases = phrases(
	`licensed under the apache license,? version 2\.0`, "Apache-2.0",
	`mozilla public license,? v(?:ersion|\.)? ?2\.0`, "MPL-2.0",
	`gnu affero general public license.{0,120}version 3.{0,200}any later version`, "AGPL-3.0-or-later",
	`gnu affero general public license(?: v3\.0 only|.{0,120}version 3)`, "AGPL-3.0-only",
	`gnu lesser general public license.{0,120}version 2\.1.{0,200}any later version`, "LGPL-2.1-or-later",
	`gnu lesser general public license.{0,120}version 2\.1`, "LGPL-2.1-only",
	`gnu lesser general public license.{0,120}version 3.{0,200}any later version`, "LGPL-3.0-or-later",
	`gnu lesser general public license.{0,120}version 3`, "LGPL-3.0-only",
	`gnu general public license.{0,120}version 2.{0,200}any later version`, "GPL-2.0-or-later",
	`gnu general public license.{0,120}version 2`, "GPL-2.0-only",
	`gnu general public license.{0,120}version 3.{0,200}any later version`, "GPL-3.0-or-later",
	`gnu general public license.{0,120}version 3`, "GPL-3.0-only",
	`licensed under the mit license|released under the mit license`, "MIT",
	`business source license`, "BUSL-1.1",
	`elastic license 2\.0|elastic license,? version 2|"elastic license 2\.0"`, "Elastic-2.0",
	`server side public license`, "SSPL-1.0",
	`redis source available license`, "LicenseRef-RSAL-2.0",
)

// choiceWords signal that multiple licenses are offered as alternatives.
var choiceWords = regexp.MustCompile(`at your (?:election|option|choice)|(?:dual|tri)[- ]licen[cs](?:ed|e)|either (?:the|of)|your choice of`)

var spaces = regexp.MustCompile(`\s+`)

func normalizeText(b []byte) string {
	return strings.TrimSpace(spaces.ReplaceAllString(strings.ToLower(string(b)), " "))
}

// ClassifyText identifies the license(s) in a license file.
func ClassifyText(text []byte) Classification {
	cov := licensecheck.Scan(text)
	var ids []string
	for _, m := range cov.Match {
		if m.IsURL {
			continue
		}
		ids = append(ids, Normalize(m.ID))
	}
	norm := normalizeText(text)
	phraseHit := false
	for _, p := range textPhrases {
		if p.re.MatchString(norm) {
			ids = append(ids, p.id)
			phraseHit = true
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	// The AGPL is the GPLv3 text plus section 13, and some projects ship
	// the GPLv3 body under an AGPL title. The title states the intent.
	if slices.Contains(ids, "AGPL-3.0-only") {
		ids = slices.DeleteFunc(ids, func(id string) bool { return id == "GPL-3.0-only" })
	}

	c := Classification{Coverage: cov.Percent, ByPhrase: phraseHit}
	if len(ids) == 0 || (cov.Percent < MinCoverage && !phraseHit) {
		return c
	}
	c.Expr = combineIDs(ids, norm)
	return c
}

// ClassifyHeader identifies the license of a source file from its first
// few KB: an SPDX-License-Identifier tag if present, else a known notice.
func ClassifyHeader(head []byte) *Expr {
	if m := spdxTag.FindSubmatch(head); m != nil {
		if e, err := Parse(cleanTag(string(m[1]))); err == nil {
			return e
		}
		return Leaf(NoAssertion)
	}
	norm := normalizeText(head)
	var ids []string
	for _, p := range headerPhrases {
		if p.re.MatchString(norm) && !coveredBy(ids, p.id) {
			ids = append(ids, p.id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	slices.Sort(ids)
	return combineIDs(ids, norm)
}

// coveredBy reports whether a weaker GPL-family match is already implied by
// an earlier "-or-later" match for the same version.
func coveredBy(ids []string, id string) bool {
	if fam, ok := strings.CutSuffix(id, "-only"); ok {
		return slices.Contains(ids, fam+"-or-later")
	}
	return false
}

var spdxTag = regexp.MustCompile(`SPDX-License-Identifier:\s*([^\r\n]+)`)

// cleanTag strips comment closers that follow the expression on the same line.
func cleanTag(s string) string {
	for _, end := range []string{"*/", "-->", "#}", "--}}", "*)", "\""} {
		if i := strings.Index(s, end); i >= 0 {
			s = s[:i]
		}
	}
	return strings.TrimSpace(s)
}

// combineIDs joins multiple licenses found in one text. They are
// alternatives (OR) when the text offers a choice, otherwise all apply (AND).
func combineIDs(ids []string, norm string) *Expr {
	leaves := make([]*Expr, len(ids))
	for i, id := range ids {
		leaves[i] = &Expr{ID: id}
	}
	if len(leaves) > 1 && choiceWords.MatchString(norm) {
		return Combine("OR", leaves...)
	}
	return Combine("AND", leaves...)
}
