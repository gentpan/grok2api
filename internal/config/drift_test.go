package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// undeclaredKeyAllow lists keys that production code reads although
// config.defaults.toml does not declare them. Entries need a reason; remove
// them once the reader is gone.
var undeclaredKeyAllow = map[string]string{}

// unreadKeyAllow lists toml keys that no production source mentions as a
// string literal. Entries need a reason; keep it empty if possible.
var unreadKeyAllow = map[string]string{}

// keyRead is one Get*/GetList call site whose key is statically known.
type keyRead struct {
	file    string
	key     string // literal key, or a glob pattern when dynamic
	dynamic bool   // key built from literals and non-literal parts; "*" marks the holes
}

// scanConfigReads parses all non-test Go files below root (skipping hidden
// directories such as .git and .claude worktrees) and returns the statically
// resolvable config key reads plus every string literal seen, so keys read
// through a thin wrapper (e.g. a local str("proxy.clearance.x")) still count.
func scanConfigReads(t *testing.T, root string) (reads []keyRead, literals map[string]bool) {
	t.Helper()
	literals = map[string]bool{}
	getters := map[string]bool{"GetStr": true, "GetInt": true, "GetBool": true, "GetList": true}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					literals[v] = true
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !getters[sel.Sel.Name] {
				return true
			}
			if key, dynamic, ok := constKey(call.Args[0]); ok {
				reads = append(reads, keyRead{file: rel, key: key, dynamic: dynamic})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return reads, literals
}

// constKey resolves a key argument made only of string literals joined with
// "+" (non-literal operands become "*"). ok is false when it has no literal at
// all (e.g. a plain variable).
func constKey(e ast.Expr) (key string, dynamic, ok bool) {
	var parts []string
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch x := e.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil {
					parts = append(parts, s)
					return
				}
			}
			parts = append(parts, "*")
		case *ast.BinaryExpr:
			if x.Op == token.ADD {
				walk(x.X)
				walk(x.Y)
				return
			}
			parts = append(parts, "*")
		default:
			parts = append(parts, "*")
		}
	}
	walk(e)
	hasLit := false
	for _, p := range parts {
		if p != "*" {
			hasLit = true
		}
		if p == "*" {
			dynamic = true
		}
	}
	if !hasLit {
		return "", false, false
	}
	return strings.Join(parts, ""), dynamic, true
}

func globToRegexp(pattern string) *regexp.Regexp {
	return regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `.+`) + "$")
}

// Drift guard: the defaults file is the single declaration of the config
// schema. Every key read in code must be declared there, and every declared
// key must be read somewhere.
func TestDefaultsFileMatchesCodeReads(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "config.defaults.toml"))
	if err != nil {
		t.Fatal(err)
	}
	defaults, err := parseTOML(raw)
	if err != nil {
		t.Fatal(err)
	}
	declared := FlattenKeys(defaults)
	declaredSet := map[string]bool{}
	for _, k := range declared {
		declaredSet[k] = true
	}

	reads, literals := scanConfigReads(t, root)
	if len(reads) < 20 {
		t.Fatalf("scanner found only %d config reads; it is probably broken", len(reads))
	}

	readSet := map[string]bool{}
	for _, r := range reads {
		if !r.dynamic {
			if _, allowed := undeclaredKeyAllow[r.key]; !allowed && !declaredSet[r.key] {
				t.Errorf("%s reads config key %q which is missing from config.defaults.toml", r.file, r.key)
			}
			readSet[r.key] = true
			continue
		}
		re := globToRegexp(r.key)
		matched := false
		for _, k := range declared {
			if re.MatchString(k) {
				readSet[k] = true
				matched = true
			}
		}
		if !matched {
			t.Errorf("%s reads dynamic config key %q but no declared key matches", r.file, r.key)
		}
	}

	var unread []string
	for _, k := range declared {
		if !readSet[k] && !literals[k] {
			if _, allowed := unreadKeyAllow[k]; !allowed {
				unread = append(unread, k)
			}
		}
	}
	sort.Strings(unread)
	for _, k := range unread {
		t.Errorf("config.defaults.toml declares %q but no code reads it", k)
	}
}

// The shipped defaults must not trip the range checks; only the two security
// notices about the unset admin/API keys are expected.
func TestShippedDefaultsValidateCleanly(t *testing.T) {
	s := &Snapshot{defaultsPath: filepath.Join("..", "..", "config.defaults.toml")}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	for _, w := range s.Validate() {
		if !strings.HasPrefix(w, "SECURITY: ") {
			t.Errorf("unexpected warning for shipped defaults: %s", w)
		}
	}
}
