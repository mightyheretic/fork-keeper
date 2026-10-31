package releasefiles

import (
	"slices"
	"testing"

	"github.com/mightyheretic/fork-keeper/internal/provider"
)

func assets(names ...string) []provider.ReleaseAsset {
	var out []provider.ReleaseAsset
	for i, n := range names {
		out = append(out, provider.ReleaseAsset{ID: int64(i + 1), Name: n})
	}
	return out
}

func names(sel []Selected) []string {
	var out []string
	for _, s := range sel {
		out = append(out, s.Name)
	}
	slices.Sort(out)
	return out
}

func TestSelect(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  []string
	}{
		{"arch packages beat everything else per CPU",
			[]string{"app-1.0-1-x86_64.pkg.tar.zst", "app-1.0-x86_64.AppImage", "app-linux-x86_64.tar.gz",
				"app_1.0_amd64.deb", "app-1.0.x86_64.rpm", "app-linux-aarch64.tar.gz", "App-1.0-arm64.pacman"},
			[]string{"app-1.0-1-x86_64.pkg.tar.zst", "App-1.0-arm64.pacman"}},
		{"portable formats when there is no Arch package",
			[]string{"App-1.0-x86_64.AppImage", "app-1.0.flatpak", "app-linux-amd64.tar.gz", "App-1.0-arm64.AppImage"},
			[]string{"App-1.0-arm64.AppImage", "App-1.0-x86_64.AppImage", "app-1.0.flatpak"}},
		{"generic Linux builds last; distro packages never",
			[]string{"tool_linux_amd64", "tool-x86_64-unknown-linux-musl.tar.xz", "tool_amd64.deb", "tool.snap",
				"tool-aarch64-unknown-linux-gnu.zip"},
			[]string{"tool-aarch64-unknown-linux-gnu.zip", "tool-x86_64-unknown-linux-musl.tar.xz", "tool_linux_amd64"}},
		{"windows x64 and arm64; no 32-bit, macOS or symbols",
			[]string{"App-Setup-1.0.exe", "app-win32-x64.zip", "app-win-arm64.msi", "app-win32-ia32.zip",
				"tool-i686-pc-windows-msvc.zip", "app-x86_64-pc-windows-msvc.zip", "app-windows-x64-pdb.zip",
				"App-1.0.dmg", "app-darwin-arm64.tar.gz", "app-macos-universal.zip"},
			[]string{"App-Setup-1.0.exe", "app-win-arm64.msi", "app-win32-x64.zip", "app-x86_64-pc-windows-msvc.zip"}},
		{"other CPUs are never taken as x86_64",
			[]string{"syncthing-linux-amd64-v2.1.7.tar.gz", "syncthing-linux-mips64le-v2.1.7.tar.gz",
				"syncthing-linux-mipsle-v2.1.7.tar.gz", "syncthing-linux-ppc64le-v2.1.7.tar.gz",
				"syncthing-linux-loong64-v2.1.7.tar.gz", "syncthing-linux-arm-v2.1.7.tar.gz",
				"syncthing-linux-386-v2.1.7.tar.gz", "rg-armv7-unknown-linux-musleabihf.tar.gz", "syncthing-windows-386-v2.1.7.zip"},
			[]string{"syncthing-linux-amd64-v2.1.7.tar.gz"}},
		{"arm-64 spelled with a dash is aarch64",
			[]string{"LocalSend-1.18.2-linux-arm-64.tar.gz", "LocalSend-1.18.2-windows-arm-64.zip", "LocalSend-1.18.2-linux-arm-64.deb"},
			[]string{"LocalSend-1.18.2-linux-arm-64.tar.gz", "LocalSend-1.18.2-windows-arm-64.zip"}},
		{"arch 'any' packages fit both CPUs",
			[]string{"theme-2.0-1-any.pkg.tar.zst", "theme-linux.tar.gz"},
			[]string{"theme-2.0-1-any.pkg.tar.zst"}},
		{"checksums and signatures of kept files, and release-wide sums",
			[]string{"app-linux-x64.tar.gz", "app-linux-x64.tar.gz.sha256", "app-linux-x64.tar.gz.sig",
				"app-linux-x64.tar.gz.DIGEST", "app_amd64.deb", "app_amd64.deb.sha256", "SHA256SUMS", "SHA256SUMS.asc", "checksums.txt"},
			[]string{"SHA256SUMS", "SHA256SUMS.asc", "app-linux-x64.tar.gz", "app-linux-x64.tar.gz.DIGEST", "app-linux-x64.tar.gz.sha256",
				"app-linux-x64.tar.gz.sig", "checksums.txt"}},
		{"nothing for Windows or Linux: no checksums either",
			[]string{"app-darwin-amd64.tar.gz", "SHA256SUMS", "source.tar.gz"},
			nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := names(Select(assets(tc.files...)))
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("got  %q\nwant %q", got, want)
			}
		})
	}
}

func TestSelectPlatforms(t *testing.T) {
	sel := Select(assets("a-1-x86_64.pkg.tar.zst", "a-linux-arm64.AppImage", "a-win-arm64.zip", "a.exe", "a.exe.sig"))
	got := map[string]string{}
	for _, s := range sel {
		got[s.Name] = s.Platform + "/" + s.Tier
	}
	want := map[string]string{"a-1-x86_64.pkg.tar.zst": LinuxX64 + "/" + TierArch, "a-linux-arm64.AppImage": LinuxARM64 + "/" + TierPortable,
		"a-win-arm64.zip": WindowsARM64 + "/", "a.exe": WindowsX64 + "/", "a.exe.sig": PlatformChecksums + "/"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
}
