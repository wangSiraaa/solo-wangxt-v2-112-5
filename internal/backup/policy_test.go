package backup_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"incbackup/internal/backup"
	"incbackup/internal/policy"
	"incbackup/internal/repo"
)

// compilePolicy is a test helper: create policy with rules, publish revision 1.
func compilePolicy(t *testing.T, e *backup.Engine, name string, rules []policy.Rule) (policyID, revisionID int64) {
	t.Helper()
	pid, rid, err := e.CreatePolicy(name, "test", "v1", rules)
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if _, err := e.PublishPolicyRevision(rid); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return pid, rid
}

func rulesJSON(t *testing.T, b []byte) []policy.Rule {
	t.Helper()
	var rs []policy.Rule
	if err := json.Unmarshal(b, &rs); err != nil {
		t.Fatal(err)
	}
	return rs
}

// Acceptance ①: no policy => the legacy full scan is byte-identical in
// behavior; this is the existing TestSnapshotRestoreVerifiesDigestAndLength
// suite, but we additionally assert no freeze row exists.
func TestNoPolicyMeansFullScanNoFreeze(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	writeTree(t, src, map[string]string{"empty": "", "x.tmp": "tmp data", "keep.txt": "k"},
		map[string]os.FileMode{"empty": 0o600})

	r, err := e.CreateSnapshotWithPolicy(src, "full", true, 0)
	if err != nil {
		t.Fatal(err)
	}
	fz, err := e.Manifest.GetFreeze(r.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if fz != nil {
		t.Fatalf("legacy scan must not freeze a policy: %+v", fz)
	}
	sel, err := e.Manifest.ListSelection(r.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sel) != 0 {
		t.Fatalf("legacy scan must leave no selection evidence, got %d rows", len(sel))
	}
	entries, err := e.Manifest.EntriesOf(r.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, en := range entries {
		names[en.RelPath] = true
	}
	for _, want := range []string{".", "empty", "x.tmp", "keep.txt"} {
		if !names[want] {
			t.Errorf("full scan must contain %q", want)
		}
	}

	// restore still works incl. empty file + restricted mode
	target := filepath.Join(dir, "out")
	if _, err := e.Restore(r.SnapshotID, target); err != nil {
		t.Fatalf("restore: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(target, "empty"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("empty file not preserved: size=%d mode=%o", fi.Size(), fi.Mode().Perm())
	}
}

// Acceptance ②: exclude *.tmp then exception keep one file; evidence lists
// both kinds of hit; restored tree matches the rules.
func TestExcludeTmpWithExceptionEvidenceAndRestore(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(filepath.Join(src, "tmpcache"), 0o755))
	writeTree(t, src, map[string]string{
		"a.tmp":              "A",
		"b.tmp":              "B",
		"keep.tmp":           "KEEP ME",
		"real.txt":           "real",
		"tmpcache/c.tmp":     "C",
		"tmpcache/notes.txt": "n",
	}, nil)

	rules := []policy.Rule{
		{Action: policy.ActionExclude, Pattern: "*.tmp"},
		{Action: policy.ActionException, Pattern: "keep.tmp"},
	}
	_, rid := compilePolicy(t, e, "no-tmp", rules)

	r, err := e.CreateSnapshotWithPolicy(src, "policy run", true, rid)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if r.Freeze == nil || r.Freeze.RevisionID != rid {
		t.Fatalf("snapshot must freeze revision %d, got %+v", rid, r.Freeze)
	}

	sel, err := e.Manifest.ListSelection(r.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]repo.SelectionRecord{}
	for _, s := range sel {
		byPath[s.RelPath] = s
	}

	// excluded tmp files: single exclude hit, decisive exclude
	for _, p := range []string{"a.tmp", "b.tmp", "tmpcache/c.tmp"} {
		rec, ok := byPath[p]
		if !ok {
			t.Fatalf("missing evidence for excluded %s", p)
		}
		if rec.Included {
			t.Errorf("%s must be excluded", p)
		}
		if len(rec.Hits) != 1 || rec.Hits[0].Action != "exclude" || !rec.Hits[0].Decisive {
			t.Errorf("%s evidence wrong: %+v", p, rec.Hits)
		}
		if rec.DecisivePattern != "*.tmp" {
			t.Errorf("%s decisive pattern=%q", p, rec.DecisivePattern)
		}
	}
	// keep.tmp: both hits listed, exception decisive
	k, ok := byPath["keep.tmp"]
	if !ok {
		t.Fatal("missing evidence for keep.tmp")
	}
	if !k.Included {
		t.Fatal("keep.tmp must be included by exception")
	}
	if len(k.Hits) != 2 {
		t.Fatalf("keep.tmp must list both hits, got %+v", k.Hits)
	}
	if k.Hits[0].Action != "exclude" || k.Hits[1].Action != "exception" || !k.Hits[1].Decisive {
		t.Fatalf("keep.tmp hits wrong: %+v", k.Hits)
	}
	if k.DecisiveAction != "exception" || k.DecisivePattern != "keep.tmp" {
		t.Fatalf("keep.tmp decisive rule wrong: %s %s", k.DecisiveAction, k.DecisivePattern)
	}

	// real.txt matched no rule -> no evidence row needed
	if _, present := byPath["real.txt"]; present {
		t.Fatal("plain included path must not produce an evidence row")
	}

	// restored tree matches the rules
	target := filepath.Join(dir, "out")
	rr, err := e.Restore(r.SnapshotID, target)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	present := map[string]bool{}
	for _, f := range rr.Verified {
		present[f.RelPath] = true
	}
	for _, mustHave := range []string{"keep.tmp", "real.txt", "tmpcache/notes.txt"} {
		if !present[mustHave] {
			t.Errorf("restore must contain %s", mustHave)
		}
	}
	for _, gone := range []string{"a.tmp", "b.tmp", "tmpcache/c.tmp"} {
		if present[gone] {
			t.Errorf("restore must not contain excluded %s", gone)
		}
		if _, err := os.Lstat(filepath.Join(target, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s must not exist on disk", gone)
		}
	}
	if _, err := os.ReadFile(filepath.Join(target, "keep.tmp")); err != nil {
		t.Fatalf("keep.tmp content unreachable: %v", err)
	}
}

// Acceptance ③: illegal rules are rejected at policy creation/update, and a
// snapshot referencing a draft or deleted revision never becomes committed.
func TestIllegalRulesRejectedNoCommittedSnapshot(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	writeTree(t, src, map[string]string{"f": "x"}, nil)

	bad := [][]policy.Rule{
		{{Action: policy.ActionExclude, Pattern: "../escape"}},
		{{Action: policy.ActionExclude, Pattern: "/abs"}},
		{{Action: policy.ActionExclude, Pattern: "."}},
		{{Action: policy.ActionInclude, Pattern: "a/../../b"}},
		{{Action: policy.ActionExclude, Pattern: "a/**b"}},
	}
	for i, rs := range bad {
		if _, _, err := e.CreatePolicy("bad-"+string(rune('a'+i)), "", "", rs); err == nil {
			t.Fatalf("case %d: illegal rules accepted: %+v", i, rs)
		} else {
			var ve *policy.ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("case %d: want ValidationError, got %T %v", i, err, err)
			}
		}
	}

	// draft revision cannot back a snapshot
	pid, draftID, err := e.CreatePolicy("drafty", "", "draft",
		[]policy.Rule{{Action: policy.ActionExclude, Pattern: "*.tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.CreateSnapshotWithPolicy(src, "draft ref", true, draftID)
	var ipe *backup.ErrInvalidPolicy
	if !errors.As(err, &ipe) {
		t.Fatalf("draft revision must be rejected before snapshot, got %v", err)
	}
	all, _ := e.Manifest.ListSnapshots()
	for _, s := range all {
		if s.Status == repo.StatusCommitted && s.PolicyRevision == draftID {
			t.Fatal("no committed snapshot may reference a draft")
		}
	}

	// retired revision cannot back a snapshot either
	if _, err := e.PublishPolicyRevision(draftID); err != nil {
		t.Fatal(err)
	}
	if err := e.RetirePolicy(pid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateSnapshotWithPolicy(src, "retired ref", true, draftID); !errors.As(err, new(*backup.ErrInvalidPolicy)) {
		t.Fatalf("retired revision must be rejected, got %v", err)
	}

	// published revision cannot be edited
	if err := e.UpdateDraft(draftID, "edit", []policy.Rule{{Action: policy.ActionExclude, Pattern: "*.log"}}); err == nil {
		t.Fatal("editing a published revision must fail")
	}

	// nonexistent revision id
	if _, err := e.CreateSnapshotWithPolicy(src, "ghost", true, 99999); !errors.As(err, new(*backup.ErrInvalidPolicy)) {
		t.Fatalf("missing revision must be rejected, got %v", err)
	}
}

// Acceptance ④a: copy + modify leaves old snapshots with their old selection
// evidence; the new revision produces different scope.
func TestCopyRevisionPreservesOldEvidence(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	writeTree(t, src, map[string]string{"a.tmp": "A", "b.log": "B", "keep.tmp": "K"}, nil)

	pid, rid1 := compilePolicy(t, e, "evolving", []policy.Rule{
		{Action: policy.ActionExclude, Pattern: "*.tmp"},
		{Action: policy.ActionException, Pattern: "keep.tmp"},
	})

	snap1, err := e.CreateSnapshotWithPolicy(src, "v1 snapshot", true, rid1)
	if err != nil {
		t.Fatal(err)
	}

	// copy revision -> new draft -> modify (also exclude *.log) -> publish
	newID, num, _, err := e.CopyRevision(rid1, "v2 draft")
	if err != nil {
		t.Fatal(err)
	}
	if num != 2 {
		t.Fatalf("new revision number=%d want 2", num)
	}
	if err := e.UpdateDraft(newID, "v2", []policy.Rule{
		{Action: policy.ActionExclude, Pattern: "*.tmp"},
		{Action: policy.ActionException, Pattern: "keep.tmp"},
		{Action: policy.ActionExclude, Pattern: "*.log"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.PublishPolicyRevision(newID); err != nil {
		t.Fatalf("publish v2: %v", err)
	}

	snap2, err := e.CreateSnapshotWithPolicy(src, "v2 snapshot", true, newID)
	if err != nil {
		t.Fatal(err)
	}

	// old snapshot still freezes revision 1 and has old evidence
	fz1, err := e.Manifest.GetFreeze(snap1.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if fz1.RevisionID != rid1 || fz1.RevisionNumber != 1 {
		t.Fatalf("old snapshot freeze changed: %+v", fz1)
	}
	if len(rulesJSON(t, fz1.RulesJSON)) != 2 {
		t.Fatal("old snapshot must keep the 2-rule document")
	}
	sel1, _ := e.Manifest.ListSelection(snap1.SnapshotID)
	hasLog := false
	for _, s := range sel1 {
		if s.RelPath == "b.log" {
			hasLog = true
		}
	}
	if hasLog {
		t.Fatal("old evidence must not mention b.log under revision 1")
	}

	// new snapshot freezes revision 2; b.log now excluded
	fz2, _ := e.Manifest.GetFreeze(snap2.SnapshotID)
	if fz2.RevisionNumber != 2 || len(rulesJSON(t, fz2.RulesJSON)) != 3 {
		t.Fatalf("new freeze wrong: %+v", fz2)
	}
	sel2, _ := e.Manifest.ListSelection(snap2.SnapshotID)
	found := false
	for _, s := range sel2 {
		if s.RelPath == "b.log" && !s.Included && s.DecisivePattern == "*.log" {
			found = true
		}
	}
	if !found {
		t.Fatalf("new evidence must show b.log excluded by *.log: %+v", sel2)
	}

	// policy lineage sanity
	revs, err := e.Manifest.ListRevisions(pid)
	if err != nil || len(revs) != 2 {
		t.Fatalf("revisions = %d, err=%v", len(revs), err)
	}
	if revs[0].Status != repo.RevPublished || revs[1].Status != repo.RevPublished {
		// old published remains published — freeze keeps a copy regardless
		t.Fatalf("unexpected statuses: %s %s", revs[0].Status, revs[1].Status)
	}
}

// Acceptance ④b: two windows concurrently publishing the SAME draft: the
// conditional draft->published UPDATE accepts exactly one. Copying forward
// remains how later versions are published.
func TestConcurrentPublishOnlyOneWins(t *testing.T) {
	e, _ := openEngine(t)
	_, d1, err := e.CreatePolicy("race", "", "d1",
		[]policy.Rule{{Action: policy.ActionExclude, Pattern: "*.tmp"}})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, errs[0] = e.PublishPolicyRevision(d1) }()
	go func() { defer wg.Done(); <-start; _, errs[1] = e.PublishPolicyRevision(d1) }()
	close(start)
	wg.Wait()

	wins, losses := 0, 0
	for _, pErr := range errs {
		if pErr == nil {
			wins++
		} else {
			losses++
			if !strings.Contains(pErr.Error(), "not a draft") &&
				!errors.Is(pErr, repo.ErrNotPublished) &&
				!errors.Is(pErr, repo.ErrNotDraft) {
				t.Errorf("loser got unexpected error: %v", pErr)
			}
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins=%d losses=%d (errs %+v)", wins, losses, errs)
	}
	rev, _ := e.Manifest.GetRevision(d1)
	if rev.Status != repo.RevPublished {
		t.Fatalf("the single draft must end published, got %s", rev.Status)
	}
}

func mustJSON(rules []policy.Rule) []byte {
	b, _ := json.Marshal(rules)
	return b
}

// Acceptance ④c: a scan that fails mid-walk still keeps the frozen policy and
// locatable failure information (named file + stage).
func TestFailedScanKeepsFreezeAndLocatableError(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	g := filepath.Join(src, "growing.tmp") // even if excluded... use .log so included
	g = filepath.Join(src, "growing.log")
	must(t, os.WriteFile(g, []byte("x\n"), 0o644))
	_, rid := compilePolicy(t, e, "exclude-tmp",
		[]policy.Rule{{Action: policy.ActionExclude, Pattern: "*.tmp"}})

	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		f, _ := os.OpenFile(g, os.O_APPEND|os.O_WRONLY, 0o644)
		defer f.Close()
		for {
			select {
			case <-stop:
				return
			default:
				f.WriteString(strings.Repeat("z", 400) + "\n")
				time.Sleep(time.Millisecond)
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)
	r, err := e.CreateSnapshotWithPolicy(src, "racing", true, rid)
	close(stop)
	wg.Wait()

	if err == nil {
		t.Fatal("continuously written file must reject the snapshot")
	}
	var rej *backup.ErrRejected
	if !errors.As(err, &rej) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
	if r.Status != repo.StatusFailed {
		t.Fatalf("status=%s want failed", r.Status)
	}
	// freeze retained
	fz, ferr := e.Manifest.GetFreeze(r.SnapshotID)
	if ferr != nil {
		t.Fatal(ferr)
	}
	if fz == nil || fz.RevisionID != rid {
		t.Fatalf("failed scan must retain freeze of rev %d, got %+v", rid, fz)
	}
	// errors locate the file
	errs, _ := e.Manifest.ListErrors(r.SnapshotID)
	if len(errs) == 0 || errs[0].Stage != "scan" || errs[0].RelPath != "growing.log" {
		t.Fatalf("failure not locatable: %+v", errs)
	}
	// cannot restore, but can still query evidence
	if _, err := e.Restore(r.SnapshotID, filepath.Join(dir, "no")); err == nil {
		t.Fatal("failed snapshot must not restore")
	}
	if _, err := e.Manifest.ListSelection(r.SnapshotID); err != nil {
		t.Fatalf("evidence must remain queryable: %v", err)
	}
}

// Excluding a directory removes the whole subtree: descendants inherit the
// ancestor exclusion and restore must not recreate the directory.
func TestExcludedDirectoryNotRecreatedOnRestore(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(filepath.Join(src, "build", "sub"), 0o755))
	writeTree(t, src, map[string]string{
		"keep.txt":         "k",
		"build/out.o":      "o",
		"build/sub/deep.o": "d",
		"build/keep.tmp":   "kt",
	}, nil)
	_, rid := compilePolicy(t, e, "no-build",
		[]policy.Rule{{Action: policy.ActionExclude, Pattern: "build/"}})

	r, err := e.CreateSnapshotWithPolicy(src, "scoped", true, rid)
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := e.Manifest.ListSelection(r.SnapshotID)
	for _, s := range sel {
		if strings.HasPrefix(s.RelPath, "build") {
			if s.Included {
				t.Errorf("%s must be excluded", s.RelPath)
			}
			if s.RelPath != "build" && s.ExcludedBy != "build" {
				t.Errorf("%s must name excluded ancestor, got %q", s.RelPath, s.ExcludedBy)
			}
		}
	}
	if len(sel) != 5 { // build, out.o, sub, deep.o, keep.tmp
		t.Fatalf("want 5 evidence rows, got %d: %+v", len(sel), sel)
	}

	target := filepath.Join(dir, "out")
	if _, err := e.Restore(r.SnapshotID, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(target, "build")); !os.IsNotExist(err) {
		t.Fatalf("excluded directory must not be recreated, err=%v", err)
	}
	if _, err := os.ReadFile(filepath.Join(target, "keep.txt")); err != nil {
		t.Fatalf("kept sibling unreachable: %v", err)
	}

	// frozen document preserves the authored "build/" so it recompiles with
	// the directory-only restriction intact
	fz, err := e.Manifest.GetFreeze(r.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	frozen := rulesJSON(t, fz.RulesJSON)
	if len(frozen) != 1 || frozen[0].Pattern != "build/" || frozen[0].Order != 0 {
		t.Fatalf("frozen rule document wrong: %+v", frozen)
	}
}

// Include rules flip the default: anything not matched is excluded, recorded
// as filter_default evidence.
func TestIncludeRulesFilterDefaultEvidence(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(filepath.Join(src, "docs"), 0o755))
	writeTree(t, src, map[string]string{
		"docs/a.md":   "a",
		"scratch.tmp": "s",
		"README":      "r",
	}, nil)
	_, rid := compilePolicy(t, e, "allowlist", []policy.Rule{
		{Action: policy.ActionInclude, Pattern: "docs/**"},
		{Action: policy.ActionInclude, Pattern: "README"},
	})
	r, err := e.CreateSnapshotWithPolicy(src, "allow", true, rid)
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := e.Manifest.ListSelection(r.SnapshotID)
	got := map[string]repo.SelectionRecord{}
	for _, s := range sel {
		got[s.RelPath] = s
	}
	sc, ok := got["scratch.tmp"]
	if !ok || sc.Included || !sc.FilterDefault {
		t.Fatalf("scratch.tmp must be filter-default excluded: %+v", got["scratch.tmp"])
	}
	if _, present := got["docs/a.md"]; present {
		t.Fatal("included-by-rule path needs no evidence row")
	}
	target := filepath.Join(dir, "out")
	if _, err := e.Restore(r.SnapshotID, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(filepath.Join(target, "docs", "a.md")); err != nil {
		t.Errorf("docs/a.md missing: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(target, "scratch.tmp")); !os.IsNotExist(err) {
		t.Error("scratch.tmp must not be restored")
	}
}

// Policy scans still never follow symlinks: an excluded-tree symlink and a
// kept symlink are both stored as links, never their targets.
func TestPolicyScanDoesNotFollowSymlinks(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	must(t, os.WriteFile(filepath.Join(src, "real.txt"), []byte("real"), 0o600))
	must(t, os.Symlink("real.txt", filepath.Join(src, "link.tmp")))

	_, rid := compilePolicy(t, e, "links",
		[]policy.Rule{{Action: policy.ActionExclude, Pattern: "*.tmp"}})
	r, err := e.CreateSnapshotWithPolicy(src, "links", true, rid)
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := e.Manifest.ListSelection(r.SnapshotID)
	found := false
	for _, s := range sel {
		if s.RelPath == "link.tmp" && !s.Included && s.KindHint == repo.KindSymlink {
			found = true
		}
	}
	if !found {
		t.Fatalf("symlink must be excluded as a symlink (not followed): %+v", sel)
	}
	target := filepath.Join(dir, "out")
	if _, err := e.Restore(r.SnapshotID, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(target, "link.tmp")); !os.IsNotExist(err) {
		t.Fatal("excluded symlink must not be recreated")
	}
	fi, err := os.Lstat(filepath.Join(target, "real.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("kept file mode=%o", fi.Mode().Perm())
	}
}

// Freezing at scan start: publishing a different revision concurrently with
// an in-flight snapshot cannot change its scope. The engine serializes
// snapshots; here we assert the freeze rows point at what each request named.
func TestFreezeIsImmutableToLaterLifecycle(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	writeTree(t, src, map[string]string{"a.tmp": "A", "b": "B"}, nil)

	pid, rid1 := compilePolicy(t, e, "immutable",
		[]policy.Rule{{Action: policy.ActionExclude, Pattern: "*.tmp"}})
	r, err := e.CreateSnapshotWithPolicy(src, "s1", true, rid1)
	if err != nil {
		t.Fatal(err)
	}

	// retire the lineage after the snapshot committed
	if err := e.RetirePolicy(pid); err != nil {
		t.Fatal(err)
	}
	fz, err := e.Manifest.GetFreeze(r.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if fz.StatusAtFreeze != repo.RevPublished || len(rulesJSON(t, fz.RulesJSON)) != 1 {
		t.Fatalf("freeze must be untouched by retirement: %+v", fz)
	}
	// restore still works from the frozen copy
	if _, err := e.Restore(r.SnapshotID, filepath.Join(dir, "out")); err != nil {
		t.Fatalf("restore after policy retirement: %v", err)
	}
}
