package account

import (
	"strconv"
	"strings"
)

// parsePasswdHome finds uid's home directory in the contents of a passwd file.
//
// It lives in an untagged file so its test runs everywhere, not only on the
// linux build that uses it: the parsing is where this whole approach can go
// quietly wrong, and "only the linux CI job covers it" is a poor place for that.
//
// A line is
//
//	login:passwd:uid:gid:gecos:home:shell
//
// so the uid is field 2 and the home directory field 5. A line with too few
// fields, a non-numeric or simply different uid, or an empty home is skipped
// rather than trusted — the caller then refuses, which is the point.
func parsePasswdHome(data string, uid int) (string, bool) {
	want := strconv.Itoa(uid)
	for line := range strings.SplitSeq(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, ":")
		if len(f) < 6 || f[2] != want {
			continue
		}
		if home := strings.TrimSpace(f[5]); home != "" {
			return home, true
		}
	}
	return "", false
}
