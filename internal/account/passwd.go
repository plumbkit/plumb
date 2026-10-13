package account

import (
	"strconv"
	"strings"
)

// parsePasswdEntry finds uid's account entry in the contents of a passwd file,
// returning its login name and home directory.
//
// It lives in an untagged file so its test runs everywhere, not only on the
// linux build that uses it: the parsing is where this whole approach can go
// quietly wrong, and "only the linux CI job covers it" is a poor place for that.
//
// A line is
//
//	login:passwd:uid:gid:gecos:home:shell
//
// so the login name is field 0, the uid field 2 and the home directory field 5.
// A line with too few fields, a non-numeric or simply different uid, or an empty
// home is skipped rather than trusted — the caller then refuses, which is the
// point. A uid that resolves with no name is still an account: its home is the
// answer that matters, and the caller then names no USER rather than inventing
// one from the environment.
func parsePasswdEntry(data string, uid int) (name, home string, ok bool) {
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
		if h := strings.TrimSpace(f[5]); h != "" {
			return strings.TrimSpace(f[0]), h, true
		}
	}
	return "", "", false
}
