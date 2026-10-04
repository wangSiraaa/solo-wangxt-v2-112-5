package repo

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func openManifest(t *testing.T) *Manifest {
	t.Helper()
	m, err := OpenManifest(filepath.Join(t.TempDir(), "manifest.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestPolicyLifecycleDraftPublishDisable(t *testing.T) {
	m := openManifest(t)
	rules := []PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.tmp"}}
	pid, revID, err := m.CreatePolicy("dev", rules)
	if err != nil {
		t.Fatal(err)
	}
	if pid == 0 || revID == 0 {
		t.Fatal("ids must be assigned")
	}

	// draft cannot be referenced by snapshots
	if _, err := m.ResolvePublishedRevision(pid, 0); !errors.Is(err, ErrRevisionNotPublished) {
		t.Fatalf("draft must not resolve, got %v", err)
	}
	if err := m.PublishRevision(pid, 1); err != nil {
		t.Fatal(err)
	}
	rv, err := m.ResolvePublishedRevision(pid, 0)
	if err != nil || rv.Revision != 1 || rv.Status != RevPublished {
		t.Fatalf("resolve = %+v, %v", rv, err)
	}
	got, _ := m.RulesOfRevision(rv.ID)
	if len(got) != 1 || got[0].Pattern != "*.tmp" || got[0].Seq != 1 {
		t.Fatalf("rules = %+v", got)
	}

	// published revision is immutable
	if err := m.ReplaceRules(rv.ID, []PolicyRule{{Seq: 1, Action: "exclude", Pattern: "x"}}); !errors.Is(err, ErrNotDraft) {
		t.Fatalf("editing a published revision must fail, got %v", err)
	}
	// re-publish is not a draft transition either
	if err := m.PublishRevision(pid, 1); !errors.Is(err, ErrNotDraft) {
		t.Fatalf("re-publish must fail, got %v", err)
	}

	// disable retires it from new snapshots
	if err := m.DisableRevision(pid, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ResolvePublishedRevision(pid, 0); !errors.Is(err, ErrRevisionNotPublished) {
		t.Fatalf("disabled revision must not resolve, got %v", err)
	}
	// rules remain on record
	got, _ = m.RulesOfRevision(rv.ID)
	if len(got) != 1 {
		t.Fatal("disabled revision must keep its rules for the record")
	}
}

func TestCopyRevisionAndLinearHistory(t *testing.T) {
	m := openManifest(t)
	if _, _, err := m.CreatePolicy("dev", []PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.tmp"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.PublishRevision(1, 1); err != nil {
		t.Fatal(err)
	}
	// copy the published head, modify the draft, publish as revision 2
	draft, copied, err := m.CopyRevision(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Revision != 2 || draft.BaseRevision != 1 || draft.Status != RevDraft {
		t.Fatalf("draft = %+v", draft)
	}
	if len(copied) != 1 || copied[0].Pattern != "*.tmp" {
		t.Fatalf("copied rules = %+v", copied)
	}
	if err := m.ReplaceRules(draft.ID, []PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.log"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.PublishRevision(1, 2); err != nil {
		t.Fatal(err)
	}
	// revision 1 is untouched: history is immutable
	rv1, _ := m.GetRevision(1, 1)
	rules1, _ := m.RulesOfRevision(rv1.ID)
	if rules1[0].Pattern != "*.tmp" {
		t.Fatalf("revision 1 mutated: %+v", rules1)
	}
	head, err := m.ResolvePublishedRevision(1, 0)
	if err != nil || head.Revision != 2 {
		t.Fatalf("head = %+v, %v", head, err)
	}
	// explicit reference to the older published revision still resolves
	if _, err := m.ResolvePublishedRevision(1, 1); err != nil {
		t.Fatalf("old published revision must stay referenceable: %v", err)
	}
}

// TestConcurrentPublishOnlyOneWins is the two-window race: two drafts copied
// from the same head, published at the same time — exactly one is accepted.
func TestConcurrentPublishOnlyOneWins(t *testing.T) {
	m := openManifest(t)
	if _, _, err := m.CreatePolicy("dev", []PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.tmp"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.PublishRevision(1, 1); err != nil {
		t.Fatal(err)
	}
	d1, _, err := m.CopyRevision(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	d2, _, err := m.CopyRevision(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, rev := range []int{d1.Revision, d2.Revision} {
		wg.Add(1)
		go func(i, rev int) {
			defer wg.Done()
			errs[i] = m.PublishRevision(1, rev)
		}(i, rev)
	}
	wg.Wait()
	var ok, conflict int
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrPublishConflict):
			conflict++
		default:
			t.Fatalf("unexpected publish error: %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("want exactly 1 success + 1 conflict, got ok=%d conflict=%d", ok, conflict)
	}
	// the loser can be re-copied from the new head and then publish
	d3, _, err := m.CopyRevision(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.PublishRevision(1, d3.Revision); err != nil {
		t.Fatalf("rebased draft must publish: %v", err)
	}
}

// TestFrozenRulesSurvivePolicyChanges: the rules frozen onto a snapshot at
// begin time are independent of anything that happens to the policy later.
func TestFrozenRulesSurvivePolicyChanges(t *testing.T) {
	m := openManifest(t)
	if _, _, err := m.CreatePolicy("dev", []PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.tmp"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.PublishRevision(1, 1); err != nil {
		t.Fatal(err)
	}
	rv, err := m.ResolvePublishedRevision(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := m.RulesOfRevision(rv.ID)
	id, err := m.BeginSnapshot("/root", 42, "msg", &FrozenPolicy{PolicyID: 1, Revision: 1, Rules: rules})
	if err != nil {
		t.Fatal(err)
	}
	// policy moves on: new revision published, old one disabled
	d, _, _ := m.CopyRevision(1, 0)
	_ = m.ReplaceRules(d.ID, []PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.log"}})
	if err := m.PublishRevision(1, 2); err != nil {
		t.Fatal(err)
	}
	if err := m.DisableRevision(1, 1); err != nil {
		t.Fatal(err)
	}
	// the pending snapshot still shows revision 1 with its original rules
	fp, err := m.SnapshotPolicy(id)
	if err != nil {
		t.Fatal(err)
	}
	if fp == nil || fp.PolicyID != 1 || fp.Revision != 1 {
		t.Fatalf("frozen policy = %+v", fp)
	}
	if len(fp.Rules) != 1 || fp.Rules[0].Pattern != "*.tmp" {
		t.Fatalf("frozen rules = %+v", fp.Rules)
	}
	// a snapshot without policy reports none
	id2, err := m.BeginSnapshot("/root", 42, "plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	fp2, err := m.SnapshotPolicy(id2)
	if err != nil || fp2 != nil {
		t.Fatalf("policy-less snapshot must have no frozen policy: %+v, %v", fp2, err)
	}
}

func TestPublishValidatesBaseOnlyAgainstPublished(t *testing.T) {
	m := openManifest(t)
	// A policy whose only revision is a draft publishes fine (base 0).
	if _, _, err := m.CreatePolicy("dev", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.PublishRevision(1, 1); err != nil {
		t.Fatal(err)
	}
	// A draft copied from rev 1, then disabled, leaves no trace in history.
	d, _, _ := m.CopyRevision(1, 0)
	if err := m.DisableRevision(1, d.Revision); err != nil {
		t.Fatal(err)
	}
	// A new draft still bases itself on revision 1 and publishes.
	d2, _, err := m.CopyRevision(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d2.BaseRevision != 1 {
		t.Fatalf("base = %d, want 1", d2.BaseRevision)
	}
	if err := m.PublishRevision(1, d2.Revision); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateFromPrePolicySchema: a database created before scan policies
// existed (snapshots table without policy columns) is upgraded in place.
func TestMigrateFromPrePolicySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE snapshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT, root_path TEXT NOT NULL,
		status TEXT NOT NULL, polynomial INTEGER NOT NULL,
		file_count INTEGER NOT NULL DEFAULT 0, dir_count INTEGER NOT NULL DEFAULT 0,
		bytes_total INTEGER NOT NULL DEFAULT 0, chunks_new INTEGER NOT NULL DEFAULT 0,
		chunks_ref INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL,
		committed_at TEXT, message TEXT NOT NULL DEFAULT '')`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO snapshots (root_path, status, polynomial, created_at)
		VALUES ('/old', 'committed', 7, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	m, err := OpenManifest(path)
	if err != nil {
		t.Fatalf("old database must open: %v", err)
	}
	defer m.Close()
	// old rows are intact and report no policy
	si, err := m.GetSnapshot(1)
	if err != nil || si.Status != StatusCommitted {
		t.Fatalf("old snapshot = %+v, %v", si, err)
	}
	if si.PolicyID != nil || si.PolicyRevision != nil {
		t.Fatalf("pre-policy snapshot must have nil policy fields: %+v", si)
	}
	// new snapshots with a frozen policy work on the upgraded database
	if _, _, err := m.CreatePolicy("dev", []PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.tmp"}}); err != nil {
		t.Fatal(err)
	}
	if err := m.PublishRevision(1, 1); err != nil {
		t.Fatal(err)
	}
	id, err := m.BeginSnapshot("/root", 7, "new", &FrozenPolicy{
		PolicyID: 1, Revision: 1,
		Rules: []PolicyRule{{Seq: 1, Action: "exclude", Pattern: "*.tmp"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	fp, err := m.SnapshotPolicy(id)
	if err != nil || fp == nil || fp.Revision != 1 {
		t.Fatalf("frozen policy on migrated db = %+v, %v", fp, err)
	}
}
