// Package releasefiles keeps copies of an upstream release's Windows and
// Linux files on a release of the fork. Files are downloaded to a
// temporary file only long enough to upload and verify them; nothing is
// kept locally.
package releasefiles

import (
	"regexp"
	"slices"
	"strings"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

// Platforms a file is kept for.
const (
	WindowsX64   = "windows-x86_64"
	WindowsARM64 = "windows-aarch64"
	LinuxX64     = "linux-x86_64"
	LinuxARM64   = "linux-aarch64"
)

// Linux tiers: for each CPU, only the best tier the release has is kept.
const (
	TierArch     = "arch"     // Arch Linux packages
	TierPortable = "portable" // AppImage and Flatpak bundles: run on Arch as-is
	TierGeneric  = "generic"  // tarballs, zips and plain binaries
)

// Selected is a file to keep.
type Selected struct {
	provider.ReleaseAsset
	// Platform is one of the platform constants, or "checksums" for a
	// checksum or signature file belonging to the kept files.
	Platform string
	// Tier is the Linux tier ("" for Windows and checksums).
	Tier string
}

// PlatformChecksums marks checksum and signature files.
const PlatformChecksums = "checksums"

var (
	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
	// Companion files: name.sha256, name.sig and so on.
	companionExt = []string{".sha256", ".sha256sum", ".sha512", ".sha512sum", ".md5", ".sig", ".asc", ".minisig",
		".digest", ".pem", ".cert", ".crt", ".sigstore", ".sigstore.json", ".bundle", ".intoto.jsonl"}
	// Release-wide checksum lists.
	sumsFile = regexp.MustCompile(`(^|[^a-z0-9])(sha(256|512)sums?|checksums?|hashes)([^a-z0-9]|$)`)
)

// Select picks the files to keep from a release: Windows x64 and ARM64
// builds, and for each Linux CPU (x86_64, aarch64) the files of the best
// tier available: Arch packages, else AppImage/Flatpak bundles, else
// generic Linux builds. .deb, .rpm, .snap and other distro packages, macOS
// and other systems, 32-bit builds and debug symbols are never kept.
// Checksum and signature files that belong to kept files come along.
func Select(assets []provider.ReleaseAsset) []Selected {
	type cand struct {
		a        provider.ReleaseAsset
		platform string
		tier     string
	}
	var windows []cand
	linux := map[string]map[string][]cand{} // cpu platform -> tier -> files
	var companions, sums []provider.ReleaseAsset
	for _, a := range assets {
		name := strings.ToLower(a.Name)
		if isSumsFile(name) {
			sums = append(sums, a)
			continue
		}
		if companionBase(name) != "" {
			companions = append(companions, a)
			continue
		}
		if isDebug(name) {
			continue
		}
		osName, tier := classify(name)
		if osName == "" {
			continue
		}
		cpus := cpusOf(name, osName, tier)
		for _, cpu := range cpus {
			switch osName {
			case "windows":
				windows = append(windows, cand{a, "windows-" + cpu, ""})
			case "linux":
				p := "linux-" + cpu
				if linux[p] == nil {
					linux[p] = map[string][]cand{}
				}
				linux[p][tier] = append(linux[p][tier], cand{a, p, tier})
			}
		}
	}

	var out []Selected
	seen := map[int64]bool{}
	add := func(c cand) {
		if seen[c.a.ID] {
			return
		}
		seen[c.a.ID] = true
		out = append(out, Selected{ReleaseAsset: c.a, Platform: c.platform, Tier: c.tier})
	}
	for _, c := range windows {
		add(c)
	}
	for _, p := range []string{LinuxX64, LinuxARM64} {
		for _, tier := range []string{TierArch, TierPortable, TierGeneric} {
			if files := linux[p][tier]; len(files) > 0 {
				for _, c := range files {
					add(c)
				}
				break
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	kept := map[string]bool{}
	for _, s := range out {
		kept[strings.ToLower(s.Name)] = true
	}
	for _, a := range companions {
		if kept[companionBase(strings.ToLower(a.Name))] {
			add(cand{a, PlatformChecksums, ""})
		}
	}
	for _, a := range sums {
		add(cand{a, PlatformChecksums, ""})
	}
	return out
}

func companionBase(name string) string {
	for _, ext := range companionExt {
		if base, ok := strings.CutSuffix(name, ext); ok && base != "" {
			return base
		}
	}
	return ""
}

func isSumsFile(name string) bool {
	if base := companionBase(name); base != "" {
		name = base
	}
	if osName, _ := classify(name); osName != "" {
		return false // a build that happens to have "hashes" in its name
	}
	return sumsFile.MatchString(name)
}

func hasToken(name string, tokens ...string) bool {
	parts := nonAlnum.Split(name, -1)
	for _, t := range tokens {
		if slices.Contains(parts, t) {
			return true
		}
	}
	return false
}

func isDebug(name string) bool {
	return strings.HasSuffix(name, ".pdb") || strings.HasSuffix(name, ".debug") ||
		hasToken(name, "debug", "dbg", "dbgsym", "debuginfo", "symbols", "pdb")
}

var (
	archPackage = regexp.MustCompile(`\.pkg\.tar(\.(zst|xz|gz|bz2))?$|\.pacman$`)
	excludedPkg = regexp.MustCompile(`\.(deb|rpm|snap|apk|ipk|flatpakref|flatpakrepo|dmg|xip)$|\.app\.(zip|tar\.gz)$`)
	windowsExt  = regexp.MustCompile(`\.(exe|msi|msix|msixbundle|appx|appxbundle)$`)
	genericExt  = regexp.MustCompile(`\.(tar\.(gz|xz|zst|bz2)|tgz|txz|tbz2?|zip|7z|gz|xz|zst|bz2|bin|run)$`)
)

// classify returns the OS a file is for ("windows", "linux" or "") and,
// for Linux, its tier.
func classify(name string) (string, string) {
	if excludedPkg.MatchString(name) || isOtherOS(name) {
		return "", ""
	}
	switch {
	case archPackage.MatchString(name) || hasToken(name, "archlinux"):
		return "linux", TierArch
	case strings.HasSuffix(name, ".appimage") || strings.HasSuffix(name, ".flatpak"):
		return "linux", TierPortable
	case windowsExt.MatchString(name) || hasToken(name, "windows", "win", "win32", "win64", "msvc", "mingw", "mingw64") ||
		strings.Contains(name, "pc-windows"):
		return "windows", ""
	case hasToken(name, "linux", "gnu", "musl") || strings.Contains(name, "unknown-linux"):
		if genericExt.MatchString(name) || !strings.Contains(lastPart(name), ".") {
			return "linux", TierGeneric
		}
	}
	return "", ""
}

func lastPart(name string) string {
	if i := strings.LastIndexAny(name, "-_"); i >= 0 {
		return name[i+1:]
	}
	return name
}

func isOtherOS(name string) bool {
	return hasToken(name, "darwin", "macos", "mac", "osx", "apple", "freebsd", "openbsd", "netbsd", "dragonfly",
		"android", "ios", "solaris", "illumos", "aix", "wasm", "wasi", "js")
}

// otherCPU matches CPU names other than x86_64 and aarch64 (which cpusOf
// checks first, so "x86" here is 32-bit).
var otherCPU = regexp.MustCompile(`^(i[3-6]86|386|x86|x32|ia32|win32|arm|armv[0-7].*|armhf|armel|armeabi.*|gnueabi.*|musleabi.*|` +
	`mips.*|ppc.*|powerpc.*|riscv.*|s390.*|sparc.*|loong.*|universal|universal2)$`)

func hasOtherCPU(name string) bool {
	for _, t := range nonAlnum.Split(name, -1) {
		if otherCPU.MatchString(t) {
			return true
		}
	}
	return false
}

// cpusOf returns the CPUs ("x86_64", "aarch64") a file is for. A file
// that names no CPU is taken as x86_64; an Arch "any" package fits both.
func cpusOf(name, osName, tier string) []string {
	switch {
	case hasToken(name, "aarch64", "arm64", "armv8") || strings.Contains(name, "arm-64") || strings.Contains(name, "arm_64"):
		return []string{"aarch64"}
	case strings.Contains(name, "x86_64"), strings.Contains(name, "x86-64"), hasToken(name, "amd64", "x64", "win64"):
		return []string{"x86_64"}
	case hasOtherCPU(name):
		return nil
	case tier == TierArch && hasToken(name, "any"):
		return []string{"x86_64", "aarch64"}
	}
	return []string{"x86_64"}
}
