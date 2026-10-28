package provider

import "testing"

func TestCompareVersions(t *testing.T) {
	ordered := []string{"v0.9", "v1.0.0-alpha", "v1.0.0-rc1", "v1.0.0-rc2", "v1.0.0", "1.0.1", "v1.2", "v1.9.9", "v1.10.0", "v2.0.0"}
	for i := range ordered {
		for j := range ordered {
			got := CompareVersions(ordered[i], ordered[j])
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func TestLooksLikePrerelease(t *testing.T) {
	for tag, want := range map[string]bool{
		"v2.0.0-rc1": true, "v1.0.0-beta.2": true, "1.0b2": true, "v3.0.0-alpha": true,
		"v1.0.0-dev": true, "2.0.0.preview1": true,
		"v1.2.3": false, "release-2024": false, "web": false, "lib-1.0": false, "v2.56.0": false,
	} {
		if got := LooksLikePrerelease(tag); got != want {
			t.Errorf("LooksLikePrerelease(%q) = %v, want %v", tag, got, want)
		}
	}
}
