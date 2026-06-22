package paths

import (
	"runtime"
	"strings"
)

// PathToURI converts an absolute filesystem path to a file:// URI. It is the
// single Windows-safe path→URI conversion for plumb: backslashes become forward
// slashes and a drive-lettered path (e.g. C:\foo) gains the extra leading slash
// the file scheme requires → file:///C:/foo. On a Unix path it is identical to
// the historical "file://" + path, so existing behaviour is preserved.
//
// PathToURI assumes an absolute path and applies no empty/round-trip policy: an
// empty string yields "file:///". Callers that must pass through an empty value
// or an already-"file://" URI (the uri-taking tools) guard that before calling.
func PathToURI(path string) string {
	return pathToURI(path, runtime.GOOS == "windows")
}

// URIToPath converts a file:// URI back to a filesystem path, the Windows-safe
// inverse of PathToURI. A value that is not a file:// URI (already a path, or
// empty) is returned unchanged, so URIToPath is a drop-in for the scattered
// strings.TrimPrefix(x, "file://") sites: on Unix it is exactly that strip, and
// on Windows it additionally drops the drive-letter's leading slash and swaps
// forward slashes back to backslashes (file:///C:/foo → C:\foo).
func URIToPath(uri string) string {
	return uriToPath(uri, runtime.GOOS == "windows")
}

// pathToURI and uriToPath carry an explicit windows flag so the cross-platform
// conversion is testable on any host (filepath.ToSlash/FromSlash are no-ops off
// their target OS and so cannot exercise the Windows branch on macOS/Linux).
func pathToURI(path string, windows bool) string {
	if windows {
		path = strings.ReplaceAll(path, `\`, "/")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return "file://" + path
}

func uriToPath(uri string, windows bool) string {
	if !strings.HasPrefix(uri, "file://") {
		return uri
	}
	p := strings.TrimPrefix(uri, "file://")
	if !windows {
		return p
	}
	if len(p) >= 3 && p[0] == '/' && isDriveLetter(p[1]) && p[2] == ':' {
		p = p[1:]
	}
	return strings.ReplaceAll(p, "/", `\`)
}

func isDriveLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
