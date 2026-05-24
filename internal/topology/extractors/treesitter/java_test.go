package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/golimpio/plumb/internal/topology"
)

var javaSrc = []byte(`package com.example.service;

import java.util.List;
import org.junit.jupiter.api.Test;

public class UserService implements Notifier {
    private static final int MAX_RETRIES = 3;
    private int counter = 0;

    public UserService(Repo repo) {
        this.repo = repo;
    }

    @Override
    public boolean notify(String message) {
        return log(message);
    }

    private boolean log(String m) {
        return !m.isEmpty();
    }
}

interface Notifier {
    boolean notify(String message);
}

enum Role {
    ADMIN, USER, GUEST
}

class CalcTest {
    @Test
    void addsTwoNumbers() {
        assertEquals(4, 2 + 2);
    }

    void helper() {}
}
`)

func TestJava_KindsExtracted(t *testing.T) {
	nodes, _, err := NewJava().Extract(context.Background(), "src/Svc.java", javaSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "java.util.List"},
		{topology.KindClass, "UserService"},
		{topology.KindType, "Notifier"},  // interface → KindType
		{topology.KindClass, "Role"},     // enum → KindClass
		{topology.KindConstant, "ADMIN"}, // enum constant
		{topology.KindClass, "CalcTest"},
		{topology.KindConstant, "MAX_RETRIES"}, // static final → constant
		{topology.KindVariable, "counter"},     // non-final field → variable
		{topology.KindMethod, "UserService"},   // constructor
		{topology.KindMethod, "notify"},
		{topology.KindMethod, "log"},
		{topology.KindMethod, "helper"},
		{topology.KindTest, "addsTwoNumbers"}, // @Test
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestJava_MethodContainmentCertain(t *testing.T) {
	nodes, edges, err := NewJava().Extract(context.Background(), "Svc.java", javaSrc)
	if err != nil {
		t.Fatal(err)
	}
	// `log` is unique to UserService (unlike `notify`, which also names the
	// interface requirement), so it pins the containment edge unambiguously.
	var svcIdx, logIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindClass && n.Name == "UserService":
			svcIdx = int64(i)
		case n.Kind == topology.KindMethod && n.Name == "log":
			logIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == svcIdx && e.ToID == logIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge UserService→log; edges=%v", edges)
}

func TestJava_CallEdgeIntraFile(t *testing.T) {
	nodes, edges, err := NewJava().Extract(context.Background(), "Svc.java", javaSrc)
	if err != nil {
		t.Fatal(err)
	}
	// The only intra-file call is log(message) from a notify() body. The callee
	// (log) is unique; the caller is name-resolved (a heuristic that, with two
	// `notify` methods sharing a name, may attribute to either), so assert on the
	// edge into log rather than its exact source.
	var logIdx int64 = -1
	for i, n := range nodes {
		if n.Kind == topology.KindMethod && n.Name == "log" {
			logIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.ToID == logIdx {
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			if nodes[e.FromID].Name != "notify" {
				t.Errorf("call into log should come from a notify method, got %q", nodes[e.FromID].Name)
			}
			return
		}
	}
	t.Errorf("no call edge →log; edges=%v", edges)
}

func TestJava_FinalVsNonFinalField(t *testing.T) {
	nodes, _, err := NewJava().Extract(context.Background(), "Svc.java", javaSrc)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(nodes, topology.KindConstant), "MAX_RETRIES") {
		t.Error("static final MAX_RETRIES should be a constant")
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "counter") {
		t.Error("non-final counter should be a variable")
	}
}

func TestJava_TestRequiresAnnotation(t *testing.T) {
	nodes, _, err := NewJava().Extract(context.Background(), "Svc.java", javaSrc)
	if err != nil {
		t.Fatal(err)
	}
	tests := names(nodes, topology.KindTest)
	if !slices.Contains(tests, "addsTwoNumbers") {
		t.Errorf("@Test addsTwoNumbers should be a test; tests=%v", tests)
	}
	if slices.Contains(tests, "helper") {
		t.Error("helper() has no @Test and must not be a test")
	}
	if !slices.Contains(names(nodes, topology.KindMethod), "helper") {
		t.Error("helper() should be a method")
	}
}

func TestJava_LocalNotExtracted(t *testing.T) {
	src := []byte("class C {\n  void m() {\n    final int local = 5;\n    int tmp = 1;\n  }\n}\n")
	nodes, _, err := NewJava().Extract(context.Background(), "C.java", src)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []topology.NodeKind{topology.KindConstant, topology.KindVariable} {
		for _, n := range []string{"local", "tmp"} {
			if slices.Contains(names(nodes, kind), n) {
				t.Errorf("local %q inside a method body must not be extracted as %s", n, kind)
			}
		}
	}
}

func TestJava_EndLineRecorded(t *testing.T) {
	nodes, _, err := NewJava().Extract(context.Background(), "Svc.java", javaSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Kind == topology.KindClass && n.Name == "UserService" {
			if n.EndLine <= n.StartLine {
				t.Errorf("UserService EndLine=%d should exceed StartLine=%d", n.EndLine, n.StartLine)
			}
			return
		}
	}
	t.Fatal("UserService node not found")
}

func TestJava_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("// just a comment\n// more\n")} {
		nodes, edges, err := NewJava().Extract(context.Background(), "e.java", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestJava_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewJava().Extract(context.Background(), "src/Svc.java", javaSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "java" {
			t.Errorf("node %q language=%q, want java", n.Name, n.Language)
		}
		if n.Path != "src/Svc.java" {
			t.Errorf("node %q path=%q, want src/Svc.java", n.Name, n.Path)
		}
	}
}

func TestJava_Extensions(t *testing.T) {
	if !slices.Contains(NewJava().Extensions(), ".java") {
		t.Error(".java missing from Java Extensions()")
	}
}
