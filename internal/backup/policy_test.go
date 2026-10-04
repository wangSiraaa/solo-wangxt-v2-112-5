package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRulePattern(t *testing.T) {
	valid := []string{
		"*.tmp", "keep.txt", "docs/*.md", "cache/**", "**/node_modules/**",
		"a?c.txt", "file[0-9].txt", "build/", "docs/**/x.log", "deep/a/b/c",
	}
	for _, p := range valid {
		if err := ValidateRulePattern(p); err != nil {
			t.Errorf("pattern %q should be valid: %v", p, err)
		}
	}
	invalid := []string{
		"",               // empty
		"/etc/passwd",    // absolute
		"../outside",     // escapes root
		"a/../../b",      // escapes root
		"a/./b",          // dot segment
		".",              // the root itself
		"a//b",           // empty segment
		`C:\windows`,     // backslash
		"*",              // matches root -> conflicts with root
		"**",             // matches everything including root
		"**/*",           // same
		"docs/**/../etc", // .. after glob
		"[unclosed",      // malformed glob
	}
	for _, p := range invalid {
		if err := ValidateRulePattern(p); err == nil {
			t.Errorf("pattern %q should be rejected", p)
		}
	}
}

func mustCompile(t *testing.T, rules ...PolicyRule) *CompiledPolicy {
	t.Helper()
	p, err := CompilePolicy(rules)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return p
}

func decide(t *testing.T, p *CompiledPolicy, rel string, isDir bool) bool {
	t.Helper()
	inc, _ := p.Decide(rel, isDir)
	return inc
}

func TestPolicyExcludeThenException(t *testing.T) {
	p := mustCompile(t,
		PolicyRule{Seq: 1, Action: ActionExclude, Pattern: "*.tmp"},
		PolicyRule{Seq: 2, Action: ActionException, Pattern: "keep.tmp"},
	)
	cases := []struct {
		rel  string
		want bool
	}{
		{"a.tmp", false},
		{"sub/dir/b.tmp", false}, // basename rule matches at any depth
		{"keep.tmp", true},       // exception re-includes
		{"sub/keep.tmp", true},   // basename exception matches at any depth
		{"keep.txt", true},       // never excluded
		{"notes.md", true},
	}
	for _, c := range cases {
		if got := decide(t, p, c.rel, false); got != c.want {
			t.Errorf("%s: included=%v want %v", c.rel, got, c.want)
		}
	}
	// evidence: exclusions and the exception hit are both recorded
	_, ev := p.Decide("a.tmp", false)
	if ev == nil || ev.Decision != "excluded" || ev.RuleSeq != 1 || ev.RulePattern != "*.tmp" {
		t.Errorf("a.tmp evidence = %+v", ev)
	}
	_, ev = p.Decide("keep.tmp", false)
	if ev == nil || ev.Decision != "exception" || ev.RuleSeq != 2 || ev.RulePattern != "keep.tmp" {
		t.Errorf("keep.tmp evidence = %+v", ev)
	}
	// untouched path produces no evidence
	if _, ev := p.Decide("notes.md", false); ev != nil {
		t.Errorf("notes.md should have no evidence, got %+v", ev)
	}
}

func TestPolicyExceptionBeforeExcludeDoesNotSave(t *testing.T) {
	// Order matters: the exception comes first, the exclude later wins.
	p := mustCompile(t,
		PolicyRule{Seq: 1, Action: ActionException, Pattern: "keep.tmp"},
		PolicyRule{Seq: 2, Action: ActionExclude, Pattern: "*.tmp"},
	)
	if decide(t, p, "keep.tmp", false) {
		t.Error("exception before exclude must not keep the file")
	}
}

func TestPolicyAnchoredVsBasename(t *testing.T) {
	p := mustCompile(t,
		PolicyRule{Seq: 1, Action: ActionExclude, Pattern: "docs/*.tmp"},
	)
	if decide(t, p, "docs/a.tmp", false) {
		t.Error("anchored rule should match docs/a.tmp")
	}
	if !decide(t, p, "sub/docs/a.tmp", false) {
		t.Error("anchored rule must not match sub/docs/a.tmp")
	}
	if !decide(t, p, "a.tmp", false) {
		t.Error("anchored rule must not match top-level a.tmp")
	}
}

func TestPolicyDoublestarAndDirOnly(t *testing.T) {
	p := mustCompile(t,
		PolicyRule{Seq: 1, Action: ActionExclude, Pattern: "cache/**"},
		PolicyRule{Seq: 2, Action: ActionExclude, Pattern: "build/"},
	)
	if decide(t, p, "cache", true) || decide(t, p, "cache/x/y", false) {
		t.Error("cache/** should match cache and everything below")
	}
	if decide(t, p, "build", true) {
		t.Error("build/ should match the directory")
	}
	if !decide(t, p, "build", false) {
		t.Error("build/ must not match a file named build")
	}
	if !decide(t, p, "buildout", true) {
		t.Error("build/ must not match buildout")
	}
}

func TestPolicyIncludeRulesFlipDefault(t *testing.T) {
	p := mustCompile(t,
		PolicyRule{Seq: 1, Action: ActionInclude, Pattern: "docs/**"},
		PolicyRule{Seq: 2, Action: ActionExclude, Pattern: "*.secret"},
	)
	if !decide(t, p, "docs/a.md", false) {
		t.Error("docs/a.md should be included")
	}
	if decide(t, p, "other/b.md", false) {
		t.Error("with include rules present, unmatched paths default to excluded")
	}
	if decide(t, p, "docs/x.secret", false) {
		t.Error("later exclude wins over earlier include")
	}
}

func TestPolicyExceptionEvidenceNeedsPriorExclude(t *testing.T) {
	p := mustCompile(t,
		PolicyRule{Seq: 1, Action: ActionException, Pattern: "special.txt"},
	)
	inc, ev := p.Decide("special.txt", false)
	if !inc {
		t.Fatal("exception should include")
	}
	if ev != nil {
		t.Errorf("exception without a matching exclude records no evidence, got %+v", ev)
	}
}

// TestScanAppliesPolicy exercises the walker: pruning, exceptions, symlink
// handling and evidence collection end to end.
func TestScanAppliesPolicy(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, content string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("keep.txt", "k")
	mk("drop.tmp", "d")
	mk("sub/keep.tmp", "exception target")
	mk("sub/drop.tmp", "d2")
	mk("cache/inner/x.bin", "cached")
	if err := os.Symlink("keep.txt", filepath.Join(root, "link.tmp")); err != nil {
		t.Fatal(err)
	}

	p := mustCompile(t,
		PolicyRule{Seq: 1, Action: ActionExclude, Pattern: "*.tmp"},
		PolicyRule{Seq: 2, Action: ActionException, Pattern: "keep.tmp"},
		PolicyRule{Seq: 3, Action: ActionExclude, Pattern: "cache/**"},
	)
	res, err := Scan(ScanOptions{Root: root, Policy: p, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("scan errors: %v", res.Errors)
	}
	got := map[string]bool{}
	for _, e := range res.Entries {
		got[e.RelPath] = true
	}
	for _, want := range []string{".", "keep.txt", "sub", "sub/keep.tmp"} {
		if !got[want] {
			t.Errorf("expected %q in entries", want)
		}
	}
	for _, notWant := range []string{"drop.tmp", "sub/drop.tmp", "cache", "cache/inner", "cache/inner/x.bin", "link.tmp"} {
		if got[notWant] {
			t.Errorf("%q should have been filtered out", notWant)
		}
	}
	// evidence: every excluded path with its rule + the exception hit
	evByPath := map[string]SelectionEvidence{}
	for _, ev := range res.Evidence {
		evByPath[ev.RelPath] = ev
	}
	for rel, seq := range map[string]int{
		"drop.tmp":     1,
		"sub/drop.tmp": 1,
		"cache":        3, // pruned directory recorded once, children never visited
		"link.tmp":     1, // symlink matched lexically by its own name
	} {
		ev, ok := evByPath[rel]
		if !ok {
			t.Errorf("no evidence for excluded %q", rel)
			continue
		}
		if ev.Decision != "excluded" || ev.RuleSeq != seq {
			t.Errorf("%q evidence = %+v", rel, ev)
		}
	}
	if ev := evByPath["cache"]; !ev.IsDir {
		t.Error("cache evidence should be marked as directory")
	}
	if ev, ok := evByPath["sub/keep.tmp"]; !ok || ev.Decision != "exception" || ev.RuleSeq != 2 {
		t.Errorf("sub/keep.tmp exception evidence = %+v (present=%v)", ev, ok)
	}
	if _, ok := evByPath["cache/inner/x.bin"]; ok {
		t.Error("children of a pruned directory must not produce individual evidence")
	}
}

// TestScanWithoutPolicyUnchanged proves the nil-policy walk is the historical
// full scan: everything is visited, nothing is recorded as evidence.
func TestScanWithoutPolicyUnchanged(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cache", "y.bin"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.tmp", filepath.Join(root, "l")); err != nil {
		t.Fatal(err)
	}
	res, err := Scan(ScanOptions{Root: root, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Evidence) != 0 {
		t.Fatalf("no policy -> no evidence, got %v", res.Evidence)
	}
	if len(res.Entries) != 5 { // . a.tmp cache cache/y.bin l
		t.Fatalf("full scan should see 5 entries, got %d", len(res.Entries))
	}
}
