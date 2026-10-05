package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestCoCallRules(t *testing.T) {
	root := repoRoot(t)
	for _, rule := range CoCallRules {
		sites, err := WalkCoCallSites(root, rule)
		if err != nil {
			t.Fatalf("rule %q: %v", rule.Name, err)
		}
		for _, s := range sites {
			if !s.Triggered {
				continue
			}
			if s.Satisfied {
				continue
			}
			reason, allowed := rule.Allowed[s.Key]
			switch {
			case !allowed:
				t.Errorf("%s: %s calls %s without %s.\n    Why: %s\n    If this write is not user-visible, add %q to Allowed with the reason.",
					s.Pos, s.Key, s.TriggerName, rule.Requires, rule.Why, s.Key)
			case strings.TrimSpace(reason) == "":
				t.Errorf("%s: allowlist entry %q has no reason recorded", s.Pos, s.Key)
			}
		}
	}
}

func TestCoCallAllowlistsAreLive(t *testing.T) {
	root := repoRoot(t)
	for _, rule := range CoCallRules {
		sites, err := WalkCoCallSites(root, rule)
		if err != nil {
			t.Fatalf("rule %q: %v", rule.Name, err)
		}
		existingKeys := map[string]bool{}
		triggeredKeys := map[string]bool{}
		for _, s := range sites {
			existingKeys[s.Key] = true
			if s.Triggered {
				triggeredKeys[s.Key] = true
			}
		}
		for key := range rule.Allowed {
			if !existingKeys[key] {
				t.Errorf("rule %q allowlists %q, but no such function exists — remove the entry",
					rule.Name, key)
			} else if !triggeredKeys[key] {
				t.Errorf("rule %q allowlists %q, but it no longer calls any trigger — remove the entry",
					rule.Name, key)
			}
		}
	}
}

func TestCoCallCheckerSelfTest(t *testing.T) {
	rule := CoCallRule{
		Name:     "test-rule",
		Triggers: []string{"safeWrite", "os.Remove"},
		Requires: "recordHistory",
		Allowed:  map[string]string{"p.T.Execute": "method is exempt"},
	}

	// Case 1: calling safeWrite without recordHistory must be flagged
	src1 := `package p
func Unsafe() {
	safeWrite("f.txt", nil)
}`
	fset1 := token.NewFileSet()
	f1, err := parser.ParseFile(fset1, "src1.go", src1, 0)
	if err != nil {
		t.Fatal(err)
	}
	s1 := InspectFuncDecl(fset1, "p", "src1.go", f1.Decls[0].(*ast.FuncDecl), rule)
	if !s1.Triggered || s1.Satisfied {
		t.Errorf("case 1: got triggered=%v satisfied=%v, want triggered=true satisfied=false", s1.Triggered, s1.Satisfied)
	}

	// Case 2: calling safeWrite with d.deps.recordHistory(c) must not be flagged (satisfied)
	src2 := `package p
func Safe(d *D, c Change) {
	safeWrite("f.txt", nil)
	d.deps.recordHistory(c)
}`
	fset2 := token.NewFileSet()
	f2, err := parser.ParseFile(fset2, "src2.go", src2, 0)
	if err != nil {
		t.Fatal(err)
	}
	s2 := InspectFuncDecl(fset2, "p", "src2.go", f2.Decls[0].(*ast.FuncDecl), rule)
	if !s2.Triggered || !s2.Satisfied {
		t.Errorf("case 2: got triggered=%v satisfied=%v, want triggered=true satisfied=true", s2.Triggered, s2.Satisfied)
	}

	// Case 3: method (*T).Execute calling os.Remove with Allowed["p.T.Execute"] set
	src3 := `package p
type T struct{}
func (t *T) Execute() {
	os.Remove("temp")
}`
	fset3 := token.NewFileSet()
	f3, err := parser.ParseFile(fset3, "src3.go", src3, 0)
	if err != nil {
		t.Fatal(err)
	}
	s3 := InspectFuncDecl(fset3, "p", "src3.go", f3.Decls[1].(*ast.FuncDecl), rule)
	if !s3.Triggered || s3.Satisfied {
		t.Errorf("case 3: got triggered=%v satisfied=%v, want triggered=true satisfied=false", s3.Triggered, s3.Satisfied)
	}
	if s3.Key != "p.T.Execute" {
		t.Errorf("case 3: key = %q, want p.T.Execute", s3.Key)
	}
	if rule.Allowed[s3.Key] == "" {
		t.Errorf("case 3: expected p.T.Execute to be allowed")
	}
}

// Each trigger needs its OWN later Requires call. A function that records one
// of its two writes used to pass, because any one Requires call anywhere in
// the body satisfied the rule (reviewer mutation: drop the dir branch's record
// in delete_file's removeTarget, and the guard stayed green).
func TestCoCallMatchesEachTrigger(t *testing.T) {
	rule := CoCallRule{Triggers: []string{"safeWrite", "os.Remove"}, Requires: "recordHistory"}
	cases := []struct {
		name, body string
		want       bool
		wantPos    string
	}{
		{"one write, one record", "safeWrite(p, nil)\n\trecordHistory(c)", true, ""},
		{"both branches record", "if dir {\n\t\tos.Remove(p)\n\t\trecordHistory(c)\n\t} else {\n\t\tsafeWrite(p, nil)\n\t\trecordHistory(c)\n\t}", true, ""},
		{"one branch forgets", "if dir {\n\t\tos.Remove(p)\n\t} else {\n\t\tsafeWrite(p, nil)\n\t\trecordHistory(c)\n\t}", false, "src.go:4"},
		{"second write after the only record", "safeWrite(p, nil)\n\trecordHistory(c)\n\tos.Remove(q)", false, "src.go:5"},
		{"record before the write", "recordHistory(c)\n\tsafeWrite(p, nil)", false, "src.go:4"},
		{"two records for one write", "safeWrite(p, nil)\n\trecordHistory(c)\n\trecordHistory(d)", true, ""},
	}
	for _, tc := range cases {
		src := "package p\nfunc F() {\n\t" + tc.body + "\n}"
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "src.go", src, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		s := InspectFuncDecl(fset, "p", "src.go", f.Decls[0].(*ast.FuncDecl), rule)
		if !s.Triggered || s.Satisfied != tc.want {
			t.Errorf("%s: triggered=%v satisfied=%v, want satisfied=%v", tc.name, s.Triggered, s.Satisfied, tc.want)
		}
		if !tc.want && s.Pos != tc.wantPos {
			t.Errorf("%s: reported at %s, want the unrecorded write at %s", tc.name, s.Pos, tc.wantPos)
		}
	}
}
