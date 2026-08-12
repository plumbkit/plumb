package treesitter

import (
	"context"
	"slices"
	"strings"
	"testing"

	tsg "github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"

	"github.com/plumbkit/plumb/internal/topology"
)

// elixirSample is one idiomatic file exercising every construct the extractor
// claims to handle, in both body spellings. It is deliberately parse-clean —
// TestElixir_SampleParsesClean asserts that — so a failure elsewhere is the
// extractor's and not the grammar's.
const elixirSample = `defmodule MyApp.Accounts.User do
  @moduledoc """
  Users of the system.
  """
  use Ecto.Schema
  import Ecto.Changeset, only: [cast: 3]
  alias MyApp.{Repo, Mailer}
  alias MyApp.Repo
  require Logger

  @behaviour MyApp.Store
  @type t :: %__MODULE__{}
  @typep internal :: map()
  @callback fetch(id :: integer()) :: {:ok, t()} | :error

  defstruct [:name, :email, active: true]

  @doc "Greets a user."
  @spec greet(String.t()) :: String.t()
  def greet(name), do: "hi " <> name

  def greet(name, opts) when is_binary(name) do
    name
    |> String.upcase()
    |> secret()
  end

  # An ordinary comment above a private helper.
  defp secret(x) do
    total = x
    total
  end

  defmacro __using__(_opts) do
    quote do
      def generated, do: :ok
    end
  end

  defmacrop only_here(x), do: x

  defguard is_even(v) when rem(v, 2) == 0

  defdelegate blank?(value), to: MyApp.Helpers

  def a <> b, do: {a, b}

  def left ~> right when is_list(left), do: right

  def no_paren arg do
    arg
  end
end

defprotocol Size do
  @doc "size of a thing"
  def size(data)
end

defimpl Size, for: BitString do
  def size(s), do: byte_size(s)
end

defimpl Size, for: [Map, List] do
  def size(m), do: map_size(m)
end

defmodule Outer do
  defmodule Inner do
    def deep, do: :ok
  end

  defexception message: "boom"
end

defmodule MyApp.UserTest do
  use ExUnit.Case, async: true

  setup do
    :ok
  end

  describe "greet/1" do
    test "says hi" do
      assert MyApp.Accounts.User.greet("a") == "hi a"
    end

    test "with context", %{conn: conn} do
      assert conn
    end
  end

  test "top level" do
    assert ~r/x/ != nil
    assert ~w(a b)a == [:a, :b]
  end

  property "round trips" do
    assert true
  end
end
`

func extractElixir(t *testing.T, path, src string) ([]topology.Node, []topology.Edge) {
	t.Helper()
	nodes, edges, err := NewElixir().Extract(context.Background(), path, []byte(src))
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	return nodes, edges
}

func elixirNode(t *testing.T, nodes []topology.Node, qualified string) topology.Node {
	t.Helper()
	for _, n := range nodes {
		if n.Qualified == qualified {
			return n
		}
	}
	t.Fatalf("no node qualified %q; have %v", qualified, elixirQualifieds(nodes))
	return topology.Node{}
}

func elixirQualifieds(nodes []topology.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Qualified)
	}
	return out
}

func TestElixir_Registration(t *testing.T) {
	e := NewElixir()
	if got := e.Language(); got != "elixir" {
		t.Errorf("Language() = %q, want elixir", got)
	}
	want := []string{".ex", ".exs"}
	got := e.Extensions()
	if len(got) != len(want) {
		t.Fatalf("Extensions() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Extensions()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestElixir_SampleParsesClean pins the grammar: the fixture below must parse
// with no ERROR and no MISSING node, so every later assertion is about the
// extractor. A MISSING node is not an ERROR node, hence both checks.
func TestElixir_SampleParsesClean(t *testing.T) {
	tree, err := tsg.NewParser(grammars.ElixirLanguage()).Parse([]byte(elixirSample))
	if err != nil || tree == nil {
		t.Fatalf("parse: %v", err)
	}
	defer tree.Release()
	var bad []string
	var rec func(n *tsg.Node)
	rec = func(n *tsg.Node) {
		if n.IsError() {
			bad = append(bad, "ERROR@"+n.Text([]byte(elixirSample)))
		}
		if n.IsMissing() {
			bad = append(bad, "MISSING@"+n.Type(grammars.ElixirLanguage()))
		}
		for _, c := range n.Children() {
			rec(c)
		}
	}
	rec(tree.RootNode())
	if len(bad) > 0 {
		t.Errorf("idiomatic sample does not parse clean: %v", bad)
	}
}

// TestElixir_Declarations is the recall table: every declaration form, keyed by
// its qualified name so arity is asserted at the same time.
func TestElixir_Declarations(t *testing.T) {
	nodes, _ := extractElixir(t, "user.ex", elixirSample)
	cases := []struct {
		qualified string
		kind      topology.NodeKind
		name      string
		sigHas    string
	}{
		{"MyApp.Accounts.User", topology.KindPackage, "User", "defmodule MyApp.Accounts.User"},
		{"MyApp.Accounts.User.t/0", topology.KindType, "t", "@type t"},
		{"MyApp.Accounts.User.internal/0", topology.KindType, "internal", "@typep internal"},
		{"MyApp.Accounts.User.fetch/1", topology.KindFunction, "fetch", "@callback fetch(id :: integer())"},
		{"MyApp.Accounts.User.name", topology.KindVariable, "name", ":name"},
		{"MyApp.Accounts.User.email", topology.KindVariable, "email", ":email"},
		{"MyApp.Accounts.User.active", topology.KindVariable, "active", "active: true"},
		{"MyApp.Accounts.User.greet/1", topology.KindFunction, "greet", "def greet(name)"},
		{"MyApp.Accounts.User.greet/2", topology.KindFunction, "greet", "when is_binary(name)"},
		{"MyApp.Accounts.User.secret/1", topology.KindFunction, "secret", "defp secret(x)"},
		{"MyApp.Accounts.User.__using__/1", topology.KindFunction, "__using__", "defmacro __using__(_opts)"},
		{"MyApp.Accounts.User.generated/0", topology.KindFunction, "generated", "def generated"},
		{"MyApp.Accounts.User.only_here/1", topology.KindFunction, "only_here", "defmacrop only_here(x)"},
		{"MyApp.Accounts.User.is_even/1", topology.KindFunction, "is_even", "defguard is_even(v) when"},
		{"MyApp.Accounts.User.blank?/1", topology.KindFunction, "blank?", "defdelegate blank?(value)"},
		{"MyApp.Accounts.User.<>/2", topology.KindFunction, "<>", "def a <> b"},
		{"MyApp.Accounts.User.~>/2", topology.KindFunction, "~>", "def left ~> right when is_list(left)"},
		{"MyApp.Accounts.User.no_paren/1", topology.KindFunction, "no_paren", "def no_paren arg"},
		{"Size", topology.KindType, "Size", "defprotocol Size"},
		{"Size.size/1", topology.KindFunction, "size", "def size(data)"},
		{"Size.BitString", topology.KindPackage, "BitString", "defimpl Size, for: BitString"},
		{"Size.BitString.size/1", topology.KindFunction, "size", "def size(s)"},
		{"Size.size/1", topology.KindFunction, "size", "def size(data)"},
		{"Outer", topology.KindPackage, "Outer", "defmodule Outer"},
		{"Outer.Inner", topology.KindPackage, "Inner", "defmodule Outer.Inner"},
		{"Outer.Inner.deep/0", topology.KindFunction, "deep", "def deep"},
		{"Outer.message", topology.KindVariable, "message", `message: "boom"`},
		{"MyApp.UserTest", topology.KindTest, "UserTest", "defmodule MyApp.UserTest"},
		{"MyApp.UserTest.greet/1", topology.KindTest, "greet/1", `describe "greet/1"`},
		{"MyApp.UserTest.says hi", topology.KindTest, "says hi", `test "says hi"`},
		{"MyApp.UserTest.with context", topology.KindTest, "with context", `test "with context"`},
		{"MyApp.UserTest.top level", topology.KindTest, "top level", `test "top level"`},
		{"MyApp.UserTest.round trips", topology.KindTest, "round trips", `property "round trips"`},
	}
	for _, tc := range cases {
		t.Run(tc.qualified, func(t *testing.T) {
			n := elixirNode(t, nodes, tc.qualified)
			if n.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", n.Kind, tc.kind)
			}
			if n.Name != tc.name {
				t.Errorf("name = %q, want %q", n.Name, tc.name)
			}
			if !strings.Contains(n.Signature, tc.sigHas) {
				t.Errorf("signature %q does not contain %q", n.Signature, tc.sigHas)
			}
			if n.Language != "elixir" || n.Path != "user.ex" {
				t.Errorf("language/path = %q/%q", n.Language, n.Path)
			}
		})
	}
}

// TestElixir_MultiClauseAreSeparateNodes pins judgment call two: the two greet
// clauses are two nodes with distinct spans, not one merged node.
func TestElixir_MultiClauseAreSeparateNodes(t *testing.T) {
	nodes, _ := extractElixir(t, "user.ex", elixirSample)
	var greets []topology.Node
	for _, n := range nodes {
		if n.Name == "greet" && n.Kind == topology.KindFunction {
			greets = append(greets, n)
		}
	}
	if len(greets) != 2 {
		t.Fatalf("got %d greet nodes, want 2 (one per clause)", len(greets))
	}
	if greets[0].StartByte == greets[1].StartByte || greets[0].EndByte >= greets[1].StartByte {
		t.Errorf("clause spans overlap: %v vs %v", greets[0], greets[1])
	}
	if greets[0].Qualified == greets[1].Qualified {
		t.Errorf("both clauses qualified %q; arity must separate them", greets[0].Qualified)
	}
}

// TestElixir_PrivateSurvivesInSignature pins judgment call one's corollary:
// topology has no private kind, so defp/defmacrop must remain legible in the
// signature or the distinction is lost entirely.
func TestElixir_PrivateSurvivesInSignature(t *testing.T) {
	nodes, _ := extractElixir(t, "user.ex", elixirSample)
	for _, tc := range []struct{ qualified, prefix string }{
		{"MyApp.Accounts.User.secret/1", "defp "},
		{"MyApp.Accounts.User.only_here/1", "defmacrop "},
		{"MyApp.Accounts.User.greet/1", "def "},
	} {
		if got := elixirNode(t, nodes, tc.qualified).Signature; !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("%s: signature %q does not start with %q", tc.qualified, got, tc.prefix)
		}
	}
}

func TestElixir_Imports(t *testing.T) {
	nodes, _ := extractElixir(t, "user.ex", elixirSample)
	var got []string
	for _, n := range nodes {
		if n.Kind == topology.KindImport {
			got = append(got, n.Name)
		}
	}
	want := []string{"Ecto.Schema", "Ecto.Changeset", "MyApp.Repo", "MyApp.Mailer", "Logger", "ExUnit.Case"}
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing import %q; got %v", w, got)
		}
	}
	// `alias MyApp.{Repo, Mailer}` then `alias MyApp.Repo` names Repo twice.
	seen := 0
	for _, g := range got {
		if g == "MyApp.Repo" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("MyApp.Repo emitted %d times, want 1 (deduplicated)", seen)
	}
}

func TestElixir_ContainmentEdges(t *testing.T) {
	nodes, edges := extractElixir(t, "user.ex", elixirSample)
	idx := map[string]int64{}
	for i, n := range nodes {
		if _, seen := idx[n.Qualified]; !seen {
			idx[n.Qualified] = int64(i)
		}
	}
	has := map[[2]int64]bool{}
	for _, e := range edges {
		if e.Kind != topology.EdgeContains {
			continue
		}
		if e.Confidence != 1.0 || e.Source != "extractor" {
			t.Errorf("containment edge %d->%d has confidence %v source %q, want 1.0/extractor",
				e.FromID, e.ToID, e.Confidence, e.Source)
		}
		has[[2]int64{e.FromID, e.ToID}] = true
	}
	for _, tc := range [][2]string{
		{"MyApp.Accounts.User", "MyApp.Accounts.User.greet/1"},
		{"MyApp.Accounts.User", "MyApp.Accounts.User.name"},
		{"MyApp.Accounts.User", "MyApp.Accounts.User.t/0"},
		{"MyApp.Accounts.User.__using__/1", "MyApp.Accounts.User.generated/0"},
		{"Size", "Size.size/1"},
		{"Size.BitString", "Size.BitString.size/1"},
		{"Outer", "Outer.Inner"},
		{"Outer.Inner", "Outer.Inner.deep/0"},
		{"MyApp.UserTest", "MyApp.UserTest.greet/1"},
		{"MyApp.UserTest.greet/1", "MyApp.UserTest.says hi"},
	} {
		from, okF := idx[tc[0]]
		to, okT := idx[tc[1]]
		if !okF || !okT {
			t.Fatalf("missing node for edge %v", tc)
		}
		if !has[[2]int64{from, to}] {
			t.Errorf("no containment edge %s -> %s", tc[0], tc[1])
		}
	}
}

func TestElixir_CallEdges(t *testing.T) {
	nodes, edges := extractElixir(t, "call.ex", `defmodule C do
  def outer(x) do
    helper(x)
  end

  def helper(x), do: x
end
`)
	var calls []topology.Edge
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls {
			calls = append(calls, e)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("got %d call edges, want 1: %v", len(calls), calls)
	}
	if got := nodes[calls[0].FromID].Name; got != "outer" {
		t.Errorf("call edge from %q, want outer", got)
	}
	if got := nodes[calls[0].ToID].Name; got != "helper" {
		t.Errorf("call edge to %q, want helper", got)
	}
	if calls[0].Confidence != 0.8 || calls[0].Source != "heuristic" {
		t.Errorf("call edge = %v/%q, want 0.8/heuristic", calls[0].Confidence, calls[0].Source)
	}
}

// TestElixir_CallEdgesIgnoreDeclarationHeads guards the one shape a
// definition-is-a-call language makes easy to get wrong: `def greet(name)`'s own
// head is a call to `greet`, so a naive pass emits a clause-to-clause self-edge.
func TestElixir_CallEdgesIgnoreDeclarationHeads(t *testing.T) {
	_, edges := extractElixir(t, "clauses.ex", `defmodule C do
  def route(:a), do: 1
  def route(:b), do: 2
  def route(_), do: 0
end
`)
	for _, e := range edges {
		if e.Kind == topology.EdgeCalls {
			t.Errorf("unexpected call edge %d->%d from a declaration head", e.FromID, e.ToID)
		}
	}
}

// TestElixir_DocSpanCarriesAttributes is the move-safety guard: @doc and @spec
// are bound to the def below them by position alone, so both must fall inside
// the doc span or a move silently re-attaches the spec to the next function.
func TestElixir_DocSpanCarriesAttributes(t *testing.T) {
	nodes, _ := extractElixir(t, "user.ex", elixirSample)
	n := elixirNode(t, nodes, "MyApp.Accounts.User.greet/1")
	if n.DocEndByte <= n.DocStartByte {
		t.Fatalf("greet/1 has no doc span")
	}
	doc := elixirSample[n.DocStartByte:n.DocEndByte]
	for _, want := range []string{"@doc", "@spec"} {
		if !strings.Contains(doc, want) {
			t.Errorf("doc span %q does not contain %q", doc, want)
		}
	}
	if n.DocEndByte > n.StartByte {
		t.Errorf("doc span [%d,%d) overlaps the declaration at %d", n.DocStartByte, n.DocEndByte, n.StartByte)
	}
	// A plain `#` comment is a doc span too.
	p := elixirNode(t, nodes, "MyApp.Accounts.User.secret/1")
	if p.DocEndByte <= p.DocStartByte || !strings.Contains(elixirSample[p.DocStartByte:p.DocEndByte], "ordinary comment") {
		t.Errorf("secret/1 doc span = [%d,%d), want the # comment above it", p.DocStartByte, p.DocEndByte)
	}
}

// TestElixir_DocSpanStopsAtBlankLine keeps a file-leading licence banner out of
// the first declaration's doc span, which is used as an edit range.
func TestElixir_DocSpanStopsAtBlankLine(t *testing.T) {
	src := `# SPDX-License-Identifier: MIT

defmodule Banner do
  def f, do: 1
end
`
	nodes, _ := extractElixir(t, "banner.ex", src)
	n := elixirNode(t, nodes, "Banner")
	if n.DocEndByte > n.DocStartByte {
		t.Errorf("banner above a blank line was swallowed as a doc span: %q",
			src[n.DocStartByte:n.DocEndByte])
	}
}

// TestElixir_ModuledocBecomesDocstring pins the deliberate asymmetry: a
// @moduledoc lives INSIDE the module, so it cannot be a doc span (an edit range
// ahead of the declaration) and becomes the Docstring instead.
func TestElixir_ModuledocBecomesDocstring(t *testing.T) {
	nodes, _ := extractElixir(t, "user.ex", elixirSample)
	n := elixirNode(t, nodes, "MyApp.Accounts.User")
	if !strings.Contains(n.Docstring, "Users of the system") {
		t.Errorf("Docstring = %q, want the @moduledoc text", n.Docstring)
	}
	if n.DocEndByte > n.DocStartByte {
		t.Errorf("@moduledoc leaked into the doc span [%d,%d)", n.DocStartByte, n.DocEndByte)
	}
}

// TestElixir_TestModuleDetection: `use ExUnit.Case` is the structural marker, so
// a test module is KindTest while a plain module stays a package even when its
// name ends in Test.
func TestElixir_TestModuleDetection(t *testing.T) {
	nodes, _ := extractElixir(t, "t.exs", elixirSample)
	if got := elixirNode(t, nodes, "MyApp.UserTest").Kind; got != topology.KindTest {
		t.Errorf("ExUnit module kind = %q, want test", got)
	}
	if got := elixirNode(t, nodes, "MyApp.Accounts.User").Kind; got != topology.KindPackage {
		t.Errorf("plain module kind = %q, want package", got)
	}
	plain, _ := extractElixir(t, "n.ex", "defmodule NotATest do\n  def f, do: 1\nend\n")
	if got := elixirNode(t, plain, "NotATest").Kind; got != topology.KindPackage {
		t.Errorf("module without use ExUnit.Case = %q, want package", got)
	}
}

// TestElixir_NoLocalsOrCallsEmitted is the suppression guard. In a language
// where a declaration is a call, the failure mode is emitting every call — so
// pipeline stages, assertions, sigils, guards and plain bindings must all be
// absent from the symbol set.
func TestElixir_NoLocalsOrCallsEmitted(t *testing.T) {
	nodes, _ := extractElixir(t, "user.ex", elixirSample)
	banned := []string{"String", "upcase", "assert", "total", "rem", "map_size", "byte_size", "cast", "setup", "quote", "x", "conn"}
	for _, n := range nodes {
		for _, b := range banned {
			if n.Name == b {
				t.Errorf("emitted %q (%s) — a call, local or macro invocation, not a declaration", b, n.Kind)
			}
		}
	}
}

// TestElixir_SpansAreValid asserts every node carries a byte-precise, in-range,
// non-inverted span and a sane line range.
func TestElixir_SpansAreValid(t *testing.T) {
	nodes, _ := extractElixir(t, "user.ex", elixirSample)
	if len(nodes) == 0 {
		t.Fatal("no nodes")
	}
	for _, n := range nodes {
		if !n.HasBytes {
			t.Errorf("%s %q: HasBytes false", n.Kind, n.Qualified)
			continue
		}
		if n.StartByte < 0 || n.EndByte > len(elixirSample) || n.StartByte >= n.EndByte {
			t.Errorf("%s %q: span [%d,%d) out of range or inverted (len %d)",
				n.Kind, n.Qualified, n.StartByte, n.EndByte, len(elixirSample))
		}
		if n.StartLine < 1 || n.EndLine < n.StartLine {
			t.Errorf("%s %q: lines %d..%d", n.Kind, n.Qualified, n.StartLine, n.EndLine)
		}
		if n.DocEndByte > n.DocStartByte && n.DocEndByte > n.StartByte {
			t.Errorf("%s %q: doc span [%d,%d) is not ahead of the declaration at %d",
				n.Kind, n.Qualified, n.DocStartByte, n.DocEndByte, n.StartByte)
		}
	}
}

// TestElixir_RecoversFromErrorNode is the ERROR-descent guard: an unclosed `do`
// wraps a file's remainder in one ERROR, and the declarations inside it are
// still real typed nodes, so the walk must descend rather than step over.
func TestElixir_RecoversFromErrorNode(t *testing.T) {
	src := `defmodule Broken do
  def before_break, do: 1

  def oops do
    case value do
      :a -> 1

  def after_break, do: 2

  def also_after(x), do: x
end
`
	tree, err := tsg.NewParser(grammars.ElixirLanguage()).Parse([]byte(src))
	if err != nil || tree == nil {
		t.Fatalf("parse: %v", err)
	}
	broken := tree.RootNode().HasError()
	tree.Release()
	if !broken {
		t.Skip("grammar parses this sample cleanly; the ERROR path is untested here")
	}
	nodes, _ := extractElixir(t, "broken.ex", src)
	// Recovery is partial by design and the boundary is the grammar's, not the
	// walk's: a def the recovery still parses as a `call` node comes back (both
	// of these do, one of them buried three levels down inside a stab_clause
	// body), while `def oops` and `def also_after` are shredded into loose
	// `identifier "def"` + `arguments` siblings of the ERROR with no call node
	// to key off. Reading those back would mean parsing the ERROR's text, which
	// is exactly the guess this extractor refuses to make.
	for _, want := range []string{"before_break", "after_break"} {
		found := false
		for _, n := range nodes {
			if n.Name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("declaration %q not recovered from the ERROR subtree; got %v", want, elixirQualifieds(nodes))
		}
	}
	for _, n := range nodes {
		if n.Name == "" || strings.ContainsAny(n.Name, " \n\t") {
			t.Errorf("recovered a junk symbol %q — a name must come from a typed node", n.Name)
		}
	}
}

// TestElixir_BodySpellingsAgree: `do … end` and `do:` differ only in where the
// body hangs, so they must produce identical symbols.
func TestElixir_BodySpellingsAgree(t *testing.T) {
	block, _ := extractElixir(t, "a.ex", "defmodule M do\n  def f(a, b) do\n    a + b\n  end\nend\n")
	inline, _ := extractElixir(t, "a.ex", "defmodule M do\n  def f(a, b), do: a + b\nend\n")
	if len(block) != len(inline) {
		t.Fatalf("node counts differ: %v vs %v", elixirQualifieds(block), elixirQualifieds(inline))
	}
	for i := range block {
		if block[i].Qualified != inline[i].Qualified || block[i].Kind != inline[i].Kind {
			t.Errorf("node %d differs: %q/%s vs %q/%s", i,
				block[i].Qualified, block[i].Kind, inline[i].Qualified, inline[i].Kind)
		}
	}
}

// TestElixir_DynamicNamesSkipped: a name built at compile time is not in the
// source, and inventing one from the ERROR-free but nameless head would be a
// guess. Nothing is emitted for the module, but the file still yields its defs.
func TestElixir_DynamicNamesSkipped(t *testing.T) {
	nodes, _ := extractElixir(t, "dyn.ex", `defmodule unquote(name) do
  def real, do: 1
end
`)
	for _, n := range nodes {
		if n.Kind == topology.KindPackage {
			t.Errorf("emitted a module %q for a compile-time name", n.Qualified)
		}
	}
	if len(nodes) == 0 {
		t.Error("dynamic module name suppressed the whole file")
	}
}

func TestElixir_EmptyAndGarbage(t *testing.T) {
	for _, src := range []string{
		"",
		"\n\n",
		"# just a comment\n",
		"defmodule",
		"def def def",
		"\x00\x01\x02 not real source {{{",
		"defmodule A do\n  def f(",
		"\xff\xfe invalid utf8 \xc3\x28",
	} {
		nodes, _, err := NewElixir().Extract(context.Background(), "x.ex", []byte(src))
		if err != nil {
			t.Errorf("Extract(%q) returned error (want graceful nil): %v", src, err)
		}
		for _, n := range nodes {
			if n.HasBytes && (n.StartByte < 0 || n.EndByte > len(src) || n.StartByte >= n.EndByte) {
				t.Errorf("Extract(%q): node %q has out-of-range span [%d,%d)", src, n.Name, n.StartByte, n.EndByte)
			}
		}
	}
}

func TestElixir_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := NewElixir().Extract(ctx, "x.ex", []byte(elixirSample)); err == nil {
		t.Error("Extract with a cancelled context returned nil error")
	}
}
