package paths

import "testing"

func TestPathToURI(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		windows bool
		want    string
	}{
		{"unix absolute", "/home/user/file.go", false, "file:///home/user/file.go"},
		{"unix root", "/", false, "file:///"},
		{"unix empty yields scheme+root", "", false, "file:///"},
		{"windows drive", `C:\Users\foo\bar.go`, true, "file:///C:/Users/foo/bar.go"},
		{"windows forward slashes", "C:/Users/foo", true, "file:///C:/Users/foo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pathToURI(tt.path, tt.windows); got != tt.want {
				t.Errorf("pathToURI(%q, %v) = %q, want %q", tt.path, tt.windows, got, tt.want)
			}
		})
	}
}

func TestURIToPath(t *testing.T) {
	tests := []struct {
		name    string
		uri     string
		windows bool
		want    string
	}{
		{"unix uri", "file:///home/user/file.go", false, "/home/user/file.go"},
		{"unix root", "file:///", false, "/"},
		{"not a uri passes through", "/already/a/path", false, "/already/a/path"},
		{"empty passes through", "", false, ""},
		{"windows drive uri", "file:///C:/Users/foo/bar.go", true, `C:\Users\foo\bar.go`},
		{"windows lowercase drive", "file:///d:/x", true, `d:\x`},
		{"windows non-drive uri keeps leading slash", "file:///srv/share", true, `\srv\share`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := uriToPath(tt.uri, tt.windows); got != tt.want {
				t.Errorf("uriToPath(%q, %v) = %q, want %q", tt.uri, tt.windows, got, tt.want)
			}
		})
	}
}

// TestRoundTrip confirms PathToURI and URIToPath invert each other on the host
// platform for a typical absolute path.
func TestRoundTrip(t *testing.T) {
	const path = "/home/user/project/main.go"
	if got := URIToPath(PathToURI(path)); got != path {
		t.Errorf("round trip: URIToPath(PathToURI(%q)) = %q", path, got)
	}
}

// TestURIToPathMatchesHistoricalStripOnUnix locks in that, off Windows,
// URIToPath is exactly the strings.TrimPrefix(x, "file://") it replaces, so the
// mass call-site migration is behaviour-preserving on the supported platforms.
func TestURIToPathMatchesHistoricalStripOnUnix(t *testing.T) {
	cases := []string{
		"file:///a/b/c.go",
		"file://relative-ish",
		"/plain/path",
		"",
		"file:///",
	}
	for _, c := range cases {
		want := c
		if len(c) >= 7 && c[:7] == "file://" {
			want = c[7:]
		}
		if got := uriToPath(c, false); got != want {
			t.Errorf("uriToPath(%q, false) = %q, want %q (historical TrimPrefix)", c, got, want)
		}
	}
}
