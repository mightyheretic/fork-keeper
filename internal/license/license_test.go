package license

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"maps"
	"slices"
	"strings"
	"testing"
)

const mitText = `MIT License

Copyright (c) 2020 Example Corp

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
`

const buslText = `Business Source License 1.1

Parameters

Licensor:             Example Corp
Licensed Work:        Example 2.0
Change Date:          Four years from the date the Licensed Work is published.
Change License:       MPL 2.0

Terms

The Licensor hereby grants you the right to copy, modify, create derivative
works, redistribute, and make non-production use of the Licensed Work.
`

const commonsClause = `
"Commons Clause" License Condition v1.0

The Software is provided to you by the Licensor under the License, as defined
below, subject to the following condition.

Without limiting other conditions in the License, the grant of rights under the
License will not include, and the License does not grant to you, the right to
Sell the Software.
`

// mapTree is an in-memory Walker.
type mapTree map[string]string

func (m mapTree) Walk(_ context.Context, fn FileFunc) error {
	for _, p := range slices.Sorted(maps.Keys(m)) {
		if err := fn(p, int64(len(m[p])), strings.NewReader(m[p])); err != nil {
			return err
		}
	}
	return nil
}

func scan(t *testing.T, tree mapTree) *Report {
	t.Helper()
	r, err := Scan(context.Background(), tree, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseAndNormalize(t *testing.T) {
	tests := map[string]string{
		"MIT":                                 "MIT",
		"mit":                                 "MIT",
		"MIT OR Apache-2.0":                   "Apache-2.0 OR MIT",
		"(Apache-2.0 or MIT)":                 "Apache-2.0 OR MIT",
		"GPL-2.0":                             "GPL-2.0-only",
		"GPL-2.0+":                            "GPL-2.0-or-later",
		"LGPL-2.1+ AND (MIT OR BSD-3-Clause)": "(BSD-3-Clause OR MIT) AND LGPL-2.1-or-later",
		"GPL-2.0-only WITH Classpath-exception-2.0": "GPL-2.0-only WITH Classpath-exception-2.0",
		"MIT AND (MIT AND ISC)":                     "ISC AND MIT",
		"licenseref-custom":                         "LicenseRef-custom",
		"BSL-1.1":                                   "BUSL-1.1",
	}
	for in, want := range tests {
		e, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if got := e.String(); got != want {
			t.Errorf("Parse(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "MIT OR", "(MIT", "MIT)", "AND MIT", "MIT WITH", "(MIT OR ISC) WITH X", "MIT ISC"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
	if got := mustParse("MIT AND (ISC OR MIT)").Licenses(); !slices.Equal(got, []string{"ISC", "MIT"}) {
		t.Errorf("Licenses() = %v", got)
	}
}

func TestParseDeclaredAliases(t *testing.T) {
	for in, want := range map[string]string{
		"Apache 2.0":  "Apache-2.0",
		"GPLv3":       "GPL-3.0-only",
		"UNLICENSED":  "LicenseRef-UNLICENSED",
		"Unlicense":   "Unlicense",
		"MIT License": "MIT",
	} {
		e, err := ParseDeclared(in)
		if err != nil || e.String() != want {
			t.Errorf("ParseDeclared(%q) = %v, %v; want %s", in, e, err, want)
		}
	}
}

func TestClassifyText(t *testing.T) {
	c := ClassifyText([]byte(mitText))
	if c.Expr == nil || c.Expr.String() != "MIT" || c.Coverage < 90 {
		t.Fatalf("MIT: %+v", c)
	}
	c = ClassifyText([]byte(buslText))
	if c.Expr == nil || c.Expr.String() != "BUSL-1.1" || !c.ByPhrase {
		t.Fatalf("BUSL: %+v", c)
	}
	c = ClassifyText([]byte(mitText + commonsClause))
	if c.Expr == nil || c.Expr.String() != "LicenseRef-Commons-Clause AND MIT" {
		t.Fatalf("MIT + Commons Clause: %v", c.Expr)
	}
	// MPL-2.0 names the AGPL as a "Secondary License"; that is not an AGPL grant.
	secondary := `1.12. "Secondary License" means either the GNU General Public License,
Version 2.0, the GNU Lesser General Public License, Version 2.1, the GNU Affero
General Public License, Version 3.0, or any later versions of those licenses.`
	if c := ClassifyText([]byte(secondary)); c.Expr != nil && slices.Contains(c.Expr.Licenses(), "AGPL-3.0-only") {
		t.Fatalf("AGPL mention misread as a grant: %v", c.Expr)
	}
	agplTitle := "GNU AFFERO GENERAL PUBLIC LICENSE\nVersion 3, 19 November 2007\n"
	if c := ClassifyText([]byte(agplTitle)); c.Expr == nil || c.Expr.String() != "AGPL-3.0-only" {
		t.Fatalf("AGPL title: %v", c.Expr)
	}
	if c := ClassifyText([]byte("All rights reserved. Do not copy.")); c.Expr != nil {
		t.Fatalf("proprietary text classified as %v", c.Expr)
	}
}

func TestClassifyHeader(t *testing.T) {
	tests := map[string]string{
		"// SPDX-License-Identifier: Apache-2.0\npackage x":                    "Apache-2.0",
		"/* SPDX-License-Identifier: MIT OR GPL-2.0+ */\n":                     "GPL-2.0-or-later OR MIT",
		"<!-- SPDX-License-Identifier: BSD-3-Clause -->":                       "BSD-3-Clause",
		"# SPDX-License-Identifier: not a (valid expr":                         NoAssertion,
		"// Licensed under the Apache License, Version 2.0 (the \"License\");": "Apache-2.0",
		"# This program is free software; you can redistribute it under the terms of the GNU General Public License as published by the Free Software Foundation; either version 2 of the License, or (at your option) any later version.": "GPL-2.0-or-later",
		"/* Licensed under the Elastic License 2.0 or the Server Side Public License, v 1; you may not use this file except in compliance with, at your election, the Elastic License 2.0 or the Server Side Public License, v 1. */":      "Elastic-2.0 OR SSPL-1.0",
	}
	for in, want := range tests {
		got := ClassifyHeader([]byte(in))
		if got == nil || got.String() != want {
			t.Errorf("ClassifyHeader(%.50q...) = %v, want %s", in, got, want)
		}
	}
	if got := ClassifyHeader([]byte("package main\n\nfunc main() {}\n")); got != nil {
		t.Errorf("no header classified as %v", got)
	}
}

func TestScanReport(t *testing.T) {
	r := scan(t, mapTree{
		"LICENSE":                 mitText,
		"ee/LICENSE.md":           buslText,
		"vendor/x/LICENSE":        buslText, // vendored: ignored
		"node_modules/y/index.js": "// SPDX-License-Identifier: GPL-3.0-only",
		"internal/license.go":     "// SPDX-License-Identifier: MIT\npackage license",
		"main.go":                 "// SPDX-License-Identifier: MIT\npackage main",
		"logo.png":                "\x89PNG\x00\x00",
		"bin.c":                   "\x00\x01// SPDX-License-Identifier: MIT",
		"package.json":            `{"name":"x","license":"Apache-2.0"}`,
	})
	if r.Declared != "MIT" {
		t.Fatalf("Declared = %q", r.Declared)
	}
	if !slices.Equal(r.Licenses, []string{"Apache-2.0", "MIT"}) {
		t.Fatalf("Licenses = %v (manifest IDs count too)", r.Licenses)
	}
	if len(r.Files) != 2 || r.Files[0].Path != "LICENSE" || !r.Files[0].Root || r.Files[1].Path != "ee/LICENSE.md" || r.Files[1].Root {
		t.Fatalf("Files = %+v", r.Files)
	}
	if r.Headers.Scanned != 2 || r.Headers.ByLicense["MIT"] != 2 || r.Headers.ByLicense["GPL-3.0-only"] != 0 {
		t.Fatalf("Headers = %+v (binary and vendored files must be skipped)", r.Headers)
	}
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "package.json declares Apache-2.0") {
		t.Fatalf("Warnings = %v", r.Warnings)
	}
}

func TestScanManifestsOnly(t *testing.T) {
	r := scan(t, mapTree{"Cargo.toml": "[package]\nname = \"x\"\nlicense = \"MIT OR Apache-2.0\"\n"})
	if r.Declared != "Apache-2.0 OR MIT" {
		t.Fatalf("Cargo: %q", r.Declared)
	}
	r = scan(t, mapTree{"pyproject.toml": "[build-system]\nlicense = \"nope\"\n[project]\nname = \"x\"\nlicense = {text = \"BSD-3-Clause\"}\n"})
	if r.Declared != "BSD-3-Clause" {
		t.Fatalf("pyproject: %q", r.Declared)
	}
	r = scan(t, mapTree{"package.json": `{"license":"SEE LICENSE IN EULA.txt"}`})
	if r.Declared != None || len(r.Manifests) != 0 {
		t.Fatalf("SEE LICENSE IN: %+v", r)
	}
	r = scan(t, mapTree{"composer.json": `{"license":["MIT","GPL-3.0-or-later"]}`})
	if r.Declared != "GPL-3.0-or-later OR MIT" {
		t.Fatalf("composer: %q", r.Declared)
	}
	r = scan(t, mapTree{"README.md": "hello"})
	if r.Declared != None || !slices.Equal(r.Licenses, []string{None}) {
		t.Fatalf("no license: %+v", r)
	}
	r = scan(t, mapTree{"LICENSE": "All rights reserved."})
	if r.Declared != NoAssertion {
		t.Fatalf("unknown license: %q", r.Declared)
	}
}

func TestCompareRelicense(t *testing.T) {
	old := scan(t, mapTree{
		"LICENSE": mitText,
		"a.go":    "// SPDX-License-Identifier: MIT\n",
		"b.go":    "// SPDX-License-Identifier: MIT\n",
	})
	cur := scan(t, mapTree{
		"LICENSE":       buslText,
		"a.go":          "// SPDX-License-Identifier: BUSL-1.1\n",
		"b.go":          "// SPDX-License-Identifier: MIT\n",
		"c.go":          "// SPDX-License-Identifier: BUSL-1.1\n",
		"ee/LICENSE.md": buslText,
	})
	d := Compare("v1", old, "v2", cur)
	if !d.Changed || d.OldDeclared != "MIT" || d.NewDeclared != "BUSL-1.1" {
		t.Fatalf("diff = %+v", d)
	}
	if !slices.Equal(d.Added, []string{"BUSL-1.1"}) || !slices.Equal(d.Removed, []string{"MIT"}) {
		t.Fatalf("added=%v removed=%v", d.Added, d.Removed)
	}
	if len(d.Files) != 2 || d.Files[0].Path != "LICENSE" || d.Files[0].Change != "relicensed" || d.Files[1].Change != "added" {
		t.Fatalf("files = %+v", d.Files)
	}
	h := d.Headers
	if !slices.Equal(h.Added, []string{"BUSL-1.1"}) || h.Relicensed != 1 || h.Samples[0].Path != "a.go" {
		t.Fatalf("headers = %+v", h)
	}
	if !strings.Contains(d.Summary(), "declared MIT -> BUSL-1.1") {
		t.Fatalf("summary = %q", d.Summary())
	}
}

func TestCompareIgnoresCopyrightBump(t *testing.T) {
	old := scan(t, mapTree{"LICENSE": mitText})
	cur := scan(t, mapTree{"LICENSE": strings.Replace(mitText, "2020", "2026", 1)})
	if d := Compare("v1", old, "v2", cur); d.Changed {
		t.Fatalf("copyright year bump flagged as a change: %s", d.Summary())
	}
}

func TestCompareCatchesAddedRider(t *testing.T) {
	old := scan(t, mapTree{"LICENSE": mitText})
	cur := scan(t, mapTree{"LICENSE": mitText + commonsClause})
	d := Compare("v1", old, "v2", cur)
	if !d.Changed || !slices.Equal(d.Added, []string{"LicenseRef-Commons-Clause"}) {
		t.Fatalf("Commons Clause not caught: %s", d.Summary())
	}
}

func TestReportRoundTrip(t *testing.T) {
	r := scan(t, mapTree{"LICENSE": mitText, "a.go": "// SPDX-License-Identifier: MIT\n"})
	b, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalReport(b)
	if err != nil {
		t.Fatal(err)
	}
	if d := Compare("a", r, "b", back); d.Changed {
		t.Fatalf("round trip changed the report: %s", d.Summary())
	}
}

func TestTarGzWalker(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	add := func(name, body string, typ byte) {
		tw.WriteHeader(&tar.Header{Name: name, Size: int64(len(body)), Typeflag: typ, Mode: 0o644, Linkname: "x"})
		io.WriteString(tw, body)
	}
	add("owner-repo-abc123/", "", tar.TypeDir)
	add("owner-repo-abc123/LICENSE", mitText, tar.TypeReg)
	add("owner-repo-abc123/src/a.go", "// SPDX-License-Identifier: MIT\n", tar.TypeReg)
	add("owner-repo-abc123/link", "", tar.TypeSymlink)
	tw.Close()
	zw.Close()

	var paths []string
	err := TarGz{R: &buf, StripComponents: 1}.Walk(context.Background(), func(p string, _ int64, _ io.Reader) error {
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(paths, []string{"LICENSE", "src/a.go"}) {
		t.Fatalf("paths = %v", paths)
	}
}
