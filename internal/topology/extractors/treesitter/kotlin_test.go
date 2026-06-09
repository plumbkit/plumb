package treesitter

import (
	"context"
	"slices"
	"testing"

	"github.com/plumbkit/plumb/internal/topology"
)

var kotlinSrc = []byte(`package com.example.service

import com.example.model.User
import kotlinx.coroutines.flow.Flow

const val MAX_RETRIES = 3
var globalCounter: Int = 0

data class Result<out T>(val value: T?, val error: String?)

sealed class State {
    object Loading : State()
    data class Loaded(val users: List<User>) : State()
}

enum class Role { ADMIN, USER, GUEST }

interface Notifier {
    fun notify(message: String): Boolean
    val channel: String
}

class UserService(private val repo: UserRepository) : Notifier {
    private var counter: Int = 0
    override val channel: String = "default"

    companion object {
        const val VERSION = "1.0"
        fun create(): UserService = UserService(UserRepository())
    }

    override fun notify(message: String): Boolean {
        counter++
        return log(message)
    }

    private fun log(m: String): Boolean = m.isNotEmpty()
}

fun User.displayName(): String = this.name

fun main() {
    val service = UserService.create()
}
`)

func TestKotlin_KindsExtracted(t *testing.T) {
	nodes, _, err := NewKotlin().Extract(context.Background(), "src/service.kt", kotlinSrc)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	cases := []struct {
		kind topology.NodeKind
		name string
	}{
		{topology.KindImport, "com.example.model.User"},
		{topology.KindConstant, "MAX_RETRIES"},
		{topology.KindVariable, "globalCounter"},
		{topology.KindClass, "Result"},
		{topology.KindClass, "State"},
		{topology.KindClass, "Loading"},  // nested object
		{topology.KindClass, "Loaded"},   // nested data class
		{topology.KindClass, "Role"},     // enum class
		{topology.KindConstant, "ADMIN"}, // enum entry
		{topology.KindType, "Notifier"},  // interface → KindType
		{topology.KindClass, "UserService"},
		{topology.KindClass, "Companion"}, // anonymous companion object
		{topology.KindConstant, "VERSION"},
		{topology.KindVariable, "counter"},
		{topology.KindConstant, "channel"}, // override val
		{topology.KindMethod, "notify"},
		{topology.KindMethod, "log"},
		{topology.KindMethod, "create"},        // companion method
		{topology.KindFunction, "displayName"}, // top-level extension fun
		{topology.KindFunction, "main"},
	}
	for _, c := range cases {
		if !slices.Contains(names(nodes, c.kind), c.name) {
			t.Errorf("kind=%s name=%q not found; got %v", c.kind, c.name, names(nodes, c.kind))
		}
	}
}

func TestKotlin_MethodContainmentCertain(t *testing.T) {
	nodes, edges, err := NewKotlin().Extract(context.Background(), "service.kt", kotlinSrc)
	if err != nil {
		t.Fatal(err)
	}
	var svcIdx, notifyIdx int64 = -1, -1
	for i, n := range nodes {
		switch {
		case n.Kind == topology.KindClass && n.Name == "UserService":
			svcIdx = int64(i)
		case n.Kind == topology.KindMethod && n.Name == "notify":
			notifyIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == svcIdx && e.ToID == notifyIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge UserService→notify; edges=%v", edges)
}

func TestKotlin_NestedClassContainment(t *testing.T) {
	nodes, edges, err := NewKotlin().Extract(context.Background(), "service.kt", kotlinSrc)
	if err != nil {
		t.Fatal(err)
	}
	var stateIdx, loadingIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "State":
			stateIdx = int64(i)
		case "Loading":
			loadingIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeContains && e.FromID == stateIdx && e.ToID == loadingIdx {
			if e.Confidence != 1.0 || e.Source != "extractor" {
				t.Errorf("nested-class contains edge conf=%v src=%q, want 1.0/extractor", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no contains edge State→Loading; edges=%v", edges)
}

func TestKotlin_CallEdgeIntraFile(t *testing.T) {
	nodes, edges, err := NewKotlin().Extract(context.Background(), "service.kt", kotlinSrc)
	if err != nil {
		t.Fatal(err)
	}
	var notifyIdx, logIdx int64 = -1, -1
	for i, n := range nodes {
		switch n.Name {
		case "notify":
			notifyIdx = int64(i)
		case "log":
			logIdx = int64(i)
		}
	}
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls && e.FromID == notifyIdx && e.ToID == logIdx {
			if e.Confidence != 0.8 || e.Source != "heuristic" {
				t.Errorf("call edge conf=%v src=%q, want 0.8/heuristic", e.Confidence, e.Source)
			}
			return
		}
	}
	t.Errorf("no call edge notify→log; edges=%v", edges)
}

func TestKotlin_ValVsVarVsConst(t *testing.T) {
	nodes, _, err := NewKotlin().Extract(context.Background(), "service.kt", kotlinSrc)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names(nodes, topology.KindConstant), "MAX_RETRIES") {
		t.Error("const val MAX_RETRIES should be a constant")
	}
	if !slices.Contains(names(nodes, topology.KindVariable), "globalCounter") {
		t.Error("var globalCounter should be a variable")
	}
	// A class name must not also surface as a property binding.
	if slices.Contains(names(nodes, topology.KindConstant), "UserService") {
		t.Error("UserService is a class, not a constant")
	}
}

func TestKotlin_TestAnnotationDetected(t *testing.T) {
	src := []byte("import org.junit.jupiter.api.Test\n\n" +
		"class CalcTest {\n" +
		"    @Test\n" +
		"    fun addsTwoNumbers() {\n" +
		"        assert(true)\n" +
		"    }\n\n" +
		"    @Test\n" +
		"    fun `handles negative numbers`() {\n" +
		"        assert(true)\n" +
		"    }\n\n" +
		"    fun helper(): Boolean = true\n" +
		"}\n")
	nodes, _, err := NewKotlin().Extract(context.Background(), "calc_test.kt", src)
	if err != nil {
		t.Fatal(err)
	}
	tests := names(nodes, topology.KindTest)
	if !slices.Contains(tests, "addsTwoNumbers") {
		t.Errorf("@Test addsTwoNumbers not detected as test; tests=%v", tests)
	}
	// Backtick-quoted method name: backticks stripped.
	if !slices.Contains(tests, "handles negative numbers") {
		t.Errorf("backtick @Test not detected (or backticks not stripped); tests=%v", tests)
	}
	// A non-annotated method is a method, not a test.
	if slices.Contains(tests, "helper") {
		t.Error("helper() has no @Test and must not be a test")
	}
	if !slices.Contains(names(nodes, topology.KindMethod), "helper") {
		t.Error("helper() should be a method")
	}
}

func TestKotlin_LocalPropertyNotExtracted(t *testing.T) {
	// `val service = ...` inside main() must not surface as a top-level symbol.
	nodes, _, err := NewKotlin().Extract(context.Background(), "service.kt", kotlinSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []topology.NodeKind{topology.KindConstant, topology.KindVariable} {
		if slices.Contains(names(nodes, kind), "service") {
			t.Errorf("local val `service` inside main() must not be extracted as %s", kind)
		}
	}
}

func TestKotlin_EndLineRecorded(t *testing.T) {
	nodes, _, err := NewKotlin().Extract(context.Background(), "service.kt", kotlinSrc)
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
	t.Fatal("UserService class node not found")
}

func TestKotlin_EmptyAndCommentOnly(t *testing.T) {
	for _, src := range [][]byte{[]byte(""), []byte("// just a comment\n// more\n")} {
		nodes, edges, err := NewKotlin().Extract(context.Background(), "e.kt", src)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if len(nodes) != 0 || len(edges) != 0 {
			t.Errorf("src=%q: want 0 nodes/edges, got %d/%d", src, len(nodes), len(edges))
		}
	}
}

func TestKotlin_LanguageAndPath(t *testing.T) {
	nodes, _, err := NewKotlin().Extract(context.Background(), "src/service.kt", kotlinSrc)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.Language != "kotlin" {
			t.Errorf("node %q language=%q, want kotlin", n.Name, n.Language)
		}
		if n.Path != "src/service.kt" {
			t.Errorf("node %q path=%q, want src/service.kt", n.Name, n.Path)
		}
	}
}

func TestKotlin_Extensions(t *testing.T) {
	exts := NewKotlin().Extensions()
	if !slices.Contains(exts, ".kt") || !slices.Contains(exts, ".kts") {
		t.Errorf("Kotlin Extensions()=%v, want .kt and .kts", exts)
	}
}
