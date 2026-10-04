package backup_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"incbackup/internal/backup"
	"incbackup/internal/repo"
)

// publishPolicy creates a policy with one published revision and returns its id.
func publishPolicy(t *testing.T, e *backup.Engine, name string, rules []repo.PolicyRule) int64 {
	t.Helper()
	pid, _, err := e.Manifest.CreatePolicy(name, rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Manifest.PublishRevision(pid, 1); err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestSnapshotWithPolicyEvidenceAndRestore(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
	writeTree(t, src, map[string]string{
		"keep.txt":        "keep me\n",
		"debug.tmp":       "transient\n",
		"sub/keep.tmp":    "precious\n",
		"sub/scratch.tmp": "scratch\n",
	}, nil)

	pid := publishPolicy(t, e, "tmp-filter", []repo.PolicyRule{
		{Seq: 1, Action: "exclude", Pattern: "*.tmp"},
		{Seq: 2, Action: "exception", Pattern: "keep.tmp"},
	})
	res, err := e.CreateSnapshotWithPolicy(src, "filtered", true, backup.PolicySelection{PolicyID: pid})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != repo.StatusCommitted {
		t.Fatalf("status=%s", res.Status)
	}

	// evidence lists both kinds of hits: exclusions and the exception
	sel, err := e.Manifest.SelectionOf(res.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]repo.SelectionRecord{}
	for _, s := range sel {
		byPath[s.RelPath] = s
	}
	for _, rel := range []string{"debug.tmp", "sub/scratch.tmp"} {
		rec, ok := byPath[rel]
		if !ok || rec.Decision != "excluded" || rec.RuleSeq != 1 || rec.RulePattern != "*.tmp" {
			t.Errorf("%s evidence = %+v (present=%v)", rel, rec, ok)
		}
	}
	rec, ok := byPath["sub/keep.tmp"]
	if !ok || rec.Decision != "exception" || rec.RuleSeq != 2 || rec.RulePattern != "keep.tmp" {
		t.Errorf("exception evidence = %+v (present=%v)", rec, ok)
	}
	// frozen policy is attached to the snapshot
	fp, err := e.Manifest.SnapshotPolicy(res.SnapshotID)
	if err != nil || fp == nil || fp.Revision != 1 || len(fp.Rules) != 2 {
		t.Fatalf("frozen policy = %+v, %v", fp, err)
	}

	// restore conforms to the rules
	target := filepath.Join(dir, "out")
	rr, err := e.Restore(res.SnapshotID, target)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Files != 2 { // keep.txt + sub/keep.tmp
		t.Fatalf("restored files = %d, want 2", rr.Files)
	}
	for _, rel := range []string{"keep.txt", "sub/keep.tmp"} {
		if _, err := os.Lstat(filepath.Join(target, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s missing from restore: %v", rel, err)
		}
	}
	for _, rel := range []string{"debug.tmp", "sub/scratch.tmp"} {
		if _, err := os.Lstat(filepath.Join(target, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("%s must not be restored", rel)
		}
	}
}

func TestOldSnapshotKeepsEvidenceAfterPolicyCopied(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	writeTree(t, src, map[string]string{"a.log": "log\n", "b.txt": "txt\n"}, nil)

	pid := publishPolicy(t, e, "logs", []repo.PolicyRule{
		{Seq: 1, Action: "exclude", Pattern: "*.log"},
	})
	r1, err := e.CreateSnapshotWithPolicy(src, "v1", true, backup.PolicySelection{PolicyID: pid})
	if err != nil {
		t.Fatal(err)
	}

	// copy + modify + publish as revision 2 (now logs are included)
	draft, _, err := e.Manifest.CopyRevision(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	must2 := e.Manifest.ReplaceRules(draft.ID, []repo.PolicyRule{
		{Seq: 1, Action: "exclude", Pattern: "*.none"},
	})
	if must2 != nil {
		t.Fatal(must2)
	}
	if err := e.Manifest.PublishRevision(pid, 2); err != nil {
		t.Fatal(err)
	}
	r2, err := e.CreateSnapshotWithPolicy(src, "v2", true, backup.PolicySelection{PolicyID: pid})
	if err != nil {
		t.Fatal(err)
	}

	// new snapshot used revision 2 and includes a.log
	si2, err := e.Manifest.GetSnapshot(r2.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if si2.PolicyRevision == nil || *si2.PolicyRevision != 2 {
		t.Fatalf("snapshot 2 policy revision = %v", si2.PolicyRevision)
	}
	sel2, _ := e.Manifest.SelectionOf(r2.SnapshotID)
	if len(sel2) != 0 {
		t.Fatalf("revision 2 excludes nothing, evidence = %+v", sel2)
	}
	entries2, _ := e.Manifest.EntriesOf(r2.SnapshotID)
	found := false
	for _, en := range entries2 {
		if en.RelPath == "a.log" {
			found = true
		}
	}
	if !found {
		t.Fatal("revision 2 must include a.log")
	}

	// old snapshot still says revision 1 and keeps its old evidence
	si1, _ := e.Manifest.GetSnapshot(r1.SnapshotID)
	if si1.PolicyRevision == nil || *si1.PolicyRevision != 1 {
		t.Fatalf("snapshot 1 policy revision = %v", si1.PolicyRevision)
	}
	sel1, _ := e.Manifest.SelectionOf(r1.SnapshotID)
	if len(sel1) != 1 || sel1[0].RelPath != "a.log" || sel1[0].RulePattern != "*.log" {
		t.Fatalf("old evidence changed: %+v", sel1)
	}
	fp1, _ := e.Manifest.SnapshotPolicy(r1.SnapshotID)
	if len(fp1.Rules) != 1 || fp1.Rules[0].Pattern != "*.log" {
		t.Fatalf("old frozen rules changed: %+v", fp1.Rules)
	}
}

func TestInterruptedSnapshotKeepsFrozenPolicy(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	writeTree(t, src, map[string]string{"a.tmp": "tmp\n", "b.txt": "txt\n"}, nil)

	pid := publishPolicy(t, e, "tmp", []repo.PolicyRule{
		{Seq: 1, Action: "exclude", Pattern: "*.tmp"},
	})
	// finish=false: process "dies" after the scan, before verification
	res, err := e.CreateSnapshotWithPolicy(src, "crash", false, backup.PolicySelection{PolicyID: pid})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != repo.StatusPending {
		t.Fatalf("status=%s", res.Status)
	}
	// the frozen policy is inspectable even though the snapshot never committed
	fp, err := e.Manifest.SnapshotPolicy(res.SnapshotID)
	if err != nil || fp == nil {
		t.Fatalf("frozen policy = %+v, %v", fp, err)
	}
	if fp.Revision != 1 || len(fp.Rules) != 1 || fp.Rules[0].Pattern != "*.tmp" {
		t.Fatalf("frozen rules = %+v", fp.Rules)
	}
	sel, _ := e.Manifest.SelectionOf(res.SnapshotID)
	if len(sel) != 1 || sel[0].RelPath != "a.tmp" || sel[0].Decision != "excluded" {
		t.Fatalf("evidence = %+v", sel)
	}
	// recovery finalizes it later
	out, err := e.RecoverPending()
	if err != nil || len(out) != 1 || out[0].Status != repo.StatusCommitted {
		t.Fatalf("recover = %+v, %v", out, err)
	}
}

func TestSnapshotRejectsUnpublishedPolicy(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	writeTree(t, src, map[string]string{"a": "x\n"}, nil)

	// draft-only policy
	pid, _, err := e.Manifest.CreatePolicy("draft", []repo.PolicyRule{
		{Seq: 1, Action: "exclude", Pattern: "*.tmp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateSnapshotWithPolicy(src, "nope", true, backup.PolicySelection{PolicyID: pid}); !errors.Is(err, repo.ErrRevisionNotPublished) {
		t.Fatalf("draft policy must be refused, got %v", err)
	}
	// disabled revision
	if err := e.Manifest.PublishRevision(pid, 1); err != nil {
		t.Fatal(err)
	}
	if err := e.Manifest.DisableRevision(pid, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateSnapshotWithPolicy(src, "nope", true, backup.PolicySelection{PolicyID: pid}); !errors.Is(err, repo.ErrRevisionNotPublished) {
		t.Fatalf("disabled revision must be refused, got %v", err)
	}
	// no snapshot rows were created for the refused requests
	snaps, _ := e.Manifest.ListSnapshots()
	for _, s := range snaps {
		if s.Status == repo.StatusCommitted {
			t.Fatalf("a committed snapshot exists despite refusal: %+v", s)
		}
	}
	if len(snaps) != 0 {
		t.Fatalf("refused requests must not create snapshot rows, got %d", len(snaps))
	}
}

// TestIllegalFrozenRulesCannotCommit simulates catalog tampering: a revision
// marked published while holding an out-of-bounds rule. Publishing validates
// rules through the API, so this can only happen via direct writes — the
// scan-start re-validation must still refuse to commit.
func TestIllegalFrozenRulesCannotCommit(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	writeTree(t, src, map[string]string{"a": "x\n"}, nil)

	if _, _, err := e.Manifest.CreatePolicy("evil", nil); err != nil {
		t.Fatal(err)
	}
	// Bypass the API and publish a revision with an illegal rule directly.
	if _, err := e.Manifest.DB().Exec(`UPDATE policy_revisions SET status = 'published'
		WHERE policy_id = 1 AND revision = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Manifest.DB().Exec(`INSERT INTO policy_rules (revision_id, seq, action, pattern)
		SELECT id, 1, 'exclude', '../escape' FROM policy_revisions
		WHERE policy_id = 1 AND revision = 1`); err != nil {
		t.Fatal(err)
	}
	res, err := e.CreateSnapshotWithPolicy(src, "should fail", true, backup.PolicySelection{PolicyID: 1})
	var rej *backup.ErrRejected
	if !errors.As(err, &rej) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
	if res.Status != repo.StatusFailed {
		t.Fatalf("status=%s, want failed", res.Status)
	}
	// failure is locatable: stage=policy, naming the illegal pattern
	errs, _ := e.Manifest.ListErrors(res.SnapshotID)
	if len(errs) == 0 || errs[0].Stage != "policy" {
		t.Fatalf("errors = %+v", errs)
	}
	found := false
	for _, se := range errs {
		if strings.Contains(se.Message, "../escape") {
			found = true
		}
	}
	if !found {
		t.Fatalf("illegal pattern not named in %+v", errs)
	}
	// and the frozen (illegal) rules are still attached for inspection
	fp, _ := e.Manifest.SnapshotPolicy(res.SnapshotID)
	if fp == nil || len(fp.Rules) != 1 || fp.Rules[0].Pattern != "../escape" {
		t.Fatalf("frozen policy = %+v", fp)
	}
	// no committed snapshot exists
	snaps, _ := e.Manifest.ListSnapshots()
	for _, s := range snaps {
		if s.Status == repo.StatusCommitted {
			t.Fatalf("illegal policy produced a committed snapshot: %+v", s)
		}
	}
}

// TestFailedScanKeepsFrozenPolicy: a scan that fails (here: an unsupported
// fifo entry) leaves a failed snapshot that still carries the frozen policy
// and the locatable scan error.
func TestFailedScanKeepsFrozenPolicy(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(src, 0o755))
	writeTree(t, src, map[string]string{"ok.txt": "fine\n"}, nil)
	if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0o644); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}
	pid := publishPolicy(t, e, "p", []repo.PolicyRule{
		{Seq: 1, Action: "exclude", Pattern: "*.tmp"},
	})
	res, err := e.CreateSnapshotWithPolicy(src, "fails", true, backup.PolicySelection{PolicyID: pid})
	var rej *backup.ErrRejected
	if !errors.As(err, &rej) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
	if res.Status != repo.StatusFailed {
		t.Fatalf("status=%s", res.Status)
	}
	fp, err := e.Manifest.SnapshotPolicy(res.SnapshotID)
	if err != nil || fp == nil || fp.Revision != 1 || len(fp.Rules) != 1 {
		t.Fatalf("failed snapshot must keep frozen policy, got %+v, %v", fp, err)
	}
	errs, _ := e.Manifest.ListErrors(res.SnapshotID)
	if len(errs) == 0 || errs[0].Stage != "scan" || errs[0].RelPath != "pipe" {
		t.Fatalf("scan error not locatable: %+v", errs)
	}
}

func TestPreviewMatchesRealScan(t *testing.T) {
	e, dir := openEngine(t)
	src := filepath.Join(dir, "src")
	must(t, os.MkdirAll(filepath.Join(src, "sub"), 0o755))
	writeTree(t, src, map[string]string{
		"a.txt":     "a\n",
		"b.tmp":     "b\n",
		"sub/c.tmp": "c\n",
		"sub/d.md":  "d\n",
	}, nil)
	rules := []backup.PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.tmp"}}
	prev, err := e.PreviewPolicy(src, rules)
	if err != nil {
		t.Fatal(err)
	}
	if prev.Files != 2 || len(prev.Evidence) != 2 {
		t.Fatalf("preview files=%d evidence=%v", prev.Files, prev.Evidence)
	}
	for _, ev := range prev.Evidence {
		if ev.Decision != "excluded" || ev.RuleSeq != 1 {
			t.Fatalf("preview evidence = %+v", ev)
		}
	}
	// real snapshot with the same rules selects the same files
	pid := publishPolicy(t, e, "p", []repo.PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.tmp"}})
	res, err := e.CreateSnapshotWithPolicy(src, "s", true, backup.PolicySelection{PolicyID: pid})
	if err != nil {
		t.Fatal(err)
	}
	si, _ := e.Manifest.GetSnapshot(res.SnapshotID)
	if si.FileCount != prev.Files {
		t.Fatalf("preview said %d files, snapshot has %d", prev.Files, si.FileCount)
	}
}
