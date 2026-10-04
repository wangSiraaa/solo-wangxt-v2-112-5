package repo

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Policy revision lifecycle: draft -> published -> disabled. Only published
// revisions may be referenced by snapshots; published revisions are
// immutable, so a snapshot's view of a policy can never change underneath it.
const (
	RevDraft     = "draft"
	RevPublished = "published"
	RevDisabled  = "disabled"
)

// Policy is a named, versioned set of scan rules.
type Policy struct {
	ID        int64
	Name      string
	CreatedAt time.Time
}

// PolicyRevision is one immutable version of a policy's rule set.
type PolicyRevision struct {
	ID           int64
	PolicyID     int64
	Revision     int
	Status       string
	BaseRevision int // published revision this draft was copied from (0 = none)
	CreatedAt    time.Time
	PublishedAt  *time.Time
}

// PolicyRule is one ordered rule row of a revision.
type PolicyRule struct {
	Seq     int
	Action  string
	Pattern string
}

const policySchemaSQL = `
CREATE TABLE IF NOT EXISTS policies (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS policy_revisions (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	policy_id     INTEGER NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
	revision      INTEGER NOT NULL,
	status        TEXT NOT NULL,
	base_revision INTEGER NOT NULL DEFAULT 0,
	created_at    TEXT NOT NULL,
	published_at  TEXT,
	UNIQUE(policy_id, revision)
);

CREATE TABLE IF NOT EXISTS policy_rules (
	revision_id INTEGER NOT NULL REFERENCES policy_revisions(id) ON DELETE CASCADE,
	seq         INTEGER NOT NULL,
	action      TEXT NOT NULL,
	pattern     TEXT NOT NULL,
	PRIMARY KEY (revision_id, seq)
);

-- Frozen copy of the rules a snapshot was scanned with, written when the
-- snapshot begins (before the walk). Even a crashed or failed snapshot keeps
-- the exact rule set and order that defined its scope.
CREATE TABLE IF NOT EXISTS snapshot_policy_rules (
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
	seq         INTEGER NOT NULL,
	action      TEXT NOT NULL,
	pattern     TEXT NOT NULL,
	PRIMARY KEY (snapshot_id, seq)
);

-- Selection evidence: every path a rule excluded, and every exception that
-- kept a path an exclude rule had matched. This is what answers "why is
-- this file not in the manifest" months later.
CREATE TABLE IF NOT EXISTS snapshot_selection (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
	rel_path    TEXT NOT NULL,
	decision    TEXT NOT NULL,   -- excluded | exception
	rule_seq    INTEGER NOT NULL,
	rule_action TEXT NOT NULL,
	rule_pattern TEXT NOT NULL,
	is_dir      INTEGER NOT NULL DEFAULT 0,
	created_at  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_selection_snap ON snapshot_selection(snapshot_id);
`

// ErrPolicyNotFound marks a missing policy or revision.
var ErrPolicyNotFound = errors.New("policy or revision not found")

// ErrPublishConflict is returned when a publish loses the race against
// another publish: the draft's base revision is no longer the policy head,
// so the draft must be re-copied from the new published revision.
var ErrPublishConflict = errors.New("publish conflict: policy head moved, copy a fresh draft from the published revision")

// ErrNotDraft is returned when trying to edit or publish a revision that is
// not a draft.
var ErrNotDraft = errors.New("revision is not a draft")

// ErrRevisionNotPublished is returned when a snapshot references a revision
// that is not currently published.
var ErrRevisionNotPublished = errors.New("policy revision is not published")

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// CreatePolicy inserts a policy and its first draft revision (base 0).
func (m *Manifest) CreatePolicy(name string, rules []PolicyRule) (policyID, revisionID int64, err error) {
	tx, err := m.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO policies (name, created_at) VALUES (?, ?)`, name, nowUTC())
	if err != nil {
		return 0, 0, fmt.Errorf("create policy: %w", err)
	}
	policyID, _ = res.LastInsertId()
	revisionID, err = insertRevision(tx, policyID, 1, 0, rules)
	if err != nil {
		return 0, 0, err
	}
	return policyID, revisionID, tx.Commit()
}

func insertRevision(tx *sql.Tx, policyID int64, revision, base int, rules []PolicyRule) (int64, error) {
	res, err := tx.Exec(`INSERT INTO policy_revisions
		(policy_id, revision, status, base_revision, created_at)
		VALUES (?, ?, ?, ?, ?)`, policyID, revision, RevDraft, base, nowUTC())
	if err != nil {
		return 0, fmt.Errorf("create revision: %w", err)
	}
	revID, _ := res.LastInsertId()
	if err := insertRules(tx, revID, rules); err != nil {
		return 0, err
	}
	return revID, nil
}

func insertRules(tx *sql.Tx, revID int64, rules []PolicyRule) error {
	for i, r := range rules {
		seq := r.Seq
		if seq == 0 {
			seq = i + 1
		}
		if _, err := tx.Exec(`INSERT INTO policy_rules (revision_id, seq, action, pattern)
			VALUES (?, ?, ?, ?)`, revID, seq, r.Action, r.Pattern); err != nil {
			return fmt.Errorf("insert rule %d: %w", seq, err)
		}
	}
	return nil
}

// GetPolicy fetches one policy by id.
func (m *Manifest) GetPolicy(id int64) (Policy, error) {
	var p Policy
	var created string
	err := m.db.QueryRow(`SELECT id, name, created_at FROM policies WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrPolicyNotFound
	}
	if err != nil {
		return p, err
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return p, nil
}

// ListPolicies returns all policies with their revisions (rules attached).
func (m *Manifest) ListPolicies() ([]Policy, map[int64][]PolicyRevision, map[int64][]PolicyRule, error) {
	rows, err := m.db.Query(`SELECT id, name, created_at FROM policies ORDER BY id`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	var policies []Policy
	for rows.Next() {
		var p Policy
		var created string
		if err := rows.Scan(&p.ID, &p.Name, &created); err != nil {
			return nil, nil, nil, err
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		policies = append(policies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}
	revs := map[int64][]PolicyRevision{}
	rulesByRev := map[int64][]PolicyRule{}
	for _, p := range policies {
		list, err := m.RevisionsOf(p.ID)
		if err != nil {
			return nil, nil, nil, err
		}
		revs[p.ID] = list
		for _, rv := range list {
			rules, err := m.RulesOfRevision(rv.ID)
			if err != nil {
				return nil, nil, nil, err
			}
			rulesByRev[rv.ID] = rules
		}
	}
	return policies, revs, rulesByRev, nil
}

// RevisionsOf lists all revisions of a policy, newest first.
func (m *Manifest) RevisionsOf(policyID int64) ([]PolicyRevision, error) {
	rows, err := m.db.Query(`SELECT id, policy_id, revision, status, base_revision, created_at, published_at
		FROM policy_revisions WHERE policy_id = ? ORDER BY revision DESC`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PolicyRevision
	for rows.Next() {
		rv, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
	}
	return out, rows.Err()
}

func scanRevision(row interface{ Scan(...any) error }) (PolicyRevision, error) {
	var rv PolicyRevision
	var created string
	var published sql.NullString
	if err := row.Scan(&rv.ID, &rv.PolicyID, &rv.Revision, &rv.Status,
		&rv.BaseRevision, &created, &published); err != nil {
		return rv, err
	}
	rv.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if published.Valid {
		t, err := time.Parse(time.RFC3339Nano, published.String)
		if err == nil {
			rv.PublishedAt = &t
		}
	}
	return rv, nil
}

// GetRevision fetches one revision of a policy by its revision number.
func (m *Manifest) GetRevision(policyID int64, revision int) (PolicyRevision, error) {
	rv, err := scanRevision(m.db.QueryRow(`SELECT id, policy_id, revision, status, base_revision, created_at, published_at
		FROM policy_revisions WHERE policy_id = ? AND revision = ?`, policyID, revision))
	if errors.Is(err, sql.ErrNoRows) {
		return rv, ErrPolicyNotFound
	}
	return rv, err
}

// RulesOfRevision returns the ordered rules of a revision.
func (m *Manifest) RulesOfRevision(revisionID int64) ([]PolicyRule, error) {
	rows, err := m.db.Query(`SELECT seq, action, pattern FROM policy_rules
		WHERE revision_id = ? ORDER BY seq`, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PolicyRule{}
	for rows.Next() {
		var r PolicyRule
		if err := rows.Scan(&r.Seq, &r.Action, &r.Pattern); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CopyRevision creates a new draft revision of the policy, copied from
// fromRevision (a revision number; 0 = latest published revision). The new
// draft records its base so publish can detect that the head moved.
func (m *Manifest) CopyRevision(policyID int64, fromRevision int) (PolicyRevision, []PolicyRule, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return PolicyRevision{}, nil, err
	}
	defer tx.Rollback()

	var src PolicyRevision
	if fromRevision == 0 {
		// Default base: the current published head.
		err = tx.QueryRow(`SELECT id, policy_id, revision, status, base_revision, created_at, published_at
			FROM policy_revisions WHERE policy_id = ? AND status = ?
			ORDER BY revision DESC LIMIT 1`, policyID, RevPublished).Scan(
			&src.ID, &src.PolicyID, &src.Revision, &src.Status, &src.BaseRevision,
			new(string), new(sql.NullString))
		if errors.Is(err, sql.ErrNoRows) {
			return PolicyRevision{}, nil, fmt.Errorf("policy %d has no published revision to copy from: %w", policyID, ErrPolicyNotFound)
		}
		if err != nil {
			return PolicyRevision{}, nil, err
		}
	} else {
		err = tx.QueryRow(`SELECT id, policy_id, revision, status, base_revision, created_at, published_at
			FROM policy_revisions WHERE policy_id = ? AND revision = ?`, policyID, fromRevision).Scan(
			&src.ID, &src.PolicyID, &src.Revision, &src.Status, &src.BaseRevision,
			new(string), new(sql.NullString))
		if errors.Is(err, sql.ErrNoRows) {
			return PolicyRevision{}, nil, ErrPolicyNotFound
		}
		if err != nil {
			return PolicyRevision{}, nil, err
		}
	}

	rrows, err := tx.Query(`SELECT seq, action, pattern FROM policy_rules
		WHERE revision_id = ? ORDER BY seq`, src.ID)
	if err != nil {
		return PolicyRevision{}, nil, err
	}
	var rules []PolicyRule
	for rrows.Next() {
		var r PolicyRule
		if err := rrows.Scan(&r.Seq, &r.Action, &r.Pattern); err != nil {
			rrows.Close()
			return PolicyRevision{}, nil, err
		}
		rules = append(rules, r)
	}
	rrows.Close()

	var next int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(revision), 0) + 1
		FROM policy_revisions WHERE policy_id = ?`, policyID).Scan(&next); err != nil {
		return PolicyRevision{}, nil, err
	}
	revID, err := insertRevision(tx, policyID, next, src.Revision, rules)
	if err != nil {
		return PolicyRevision{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return PolicyRevision{}, nil, err
	}
	return PolicyRevision{ID: revID, PolicyID: policyID, Revision: next,
		Status: RevDraft, BaseRevision: src.Revision, CreatedAt: time.Now().UTC()}, rules, nil
}

// ReplaceRules swaps the whole rule set of a draft revision. Published and
// disabled revisions are immutable: once a snapshot can reference a
// revision, its rules are fixed forever.
func (m *Manifest) ReplaceRules(revisionID int64, rules []PolicyRule) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRow(`SELECT status FROM policy_revisions WHERE id = ?`, revisionID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrPolicyNotFound
	}
	if err != nil {
		return err
	}
	if status != RevDraft {
		return fmt.Errorf("%w (status %s): published revisions are immutable, copy a new draft instead", ErrNotDraft, status)
	}
	if _, err := tx.Exec(`DELETE FROM policy_rules WHERE revision_id = ?`, revisionID); err != nil {
		return err
	}
	if err := insertRules(tx, revisionID, rules); err != nil {
		return err
	}
	return tx.Commit()
}

// PublishRevision atomically flips a draft to published, but only when its
// base revision is still the policy's published head. Two drafts copied from
// the same head cannot both publish: the first wins, the second gets
// ErrPublishConflict and must be re-copied — history stays linear, so a
// revision number always means exactly one rule set.
func (m *Manifest) PublishRevision(policyID int64, revision int) error {
	res, err := m.db.Exec(`UPDATE policy_revisions
		SET status = ?, published_at = ?
		WHERE policy_id = ? AND revision = ? AND status = ?
		  AND base_revision = (
			SELECT COALESCE(MAX(revision), 0) FROM policy_revisions p2
			WHERE p2.policy_id = policy_revisions.policy_id AND p2.status = ?
		  )`,
		RevPublished, nowUTC(), policyID, revision, RevDraft, RevPublished)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 1 {
		return nil
	}
	rv, err := m.GetRevision(policyID, revision)
	if errors.Is(err, ErrPolicyNotFound) {
		return ErrPolicyNotFound
	}
	if err != nil {
		return err
	}
	if rv.Status != RevDraft {
		return fmt.Errorf("%w (status %s)", ErrNotDraft, rv.Status)
	}
	return ErrPublishConflict
}

// DisableRevision retires a revision (draft or published). Disabled
// revisions keep their rules for the record but can no longer be referenced
// by new snapshots; existing snapshots keep their frozen copies regardless.
func (m *Manifest) DisableRevision(policyID int64, revision int) error {
	res, err := m.db.Exec(`UPDATE policy_revisions SET status = ?
		WHERE policy_id = ? AND revision = ? AND status IN (?, ?)`,
		RevDisabled, policyID, revision, RevDraft, RevPublished)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		if _, err := m.GetRevision(policyID, revision); errors.Is(err, ErrPolicyNotFound) {
			return ErrPolicyNotFound
		}
		return fmt.Errorf("revision %d cannot be disabled from its current status", revision)
	}
	return nil
}

// ResolvePublishedRevision returns the revision a snapshot may use: the
// requested revision number, or the latest published one when revision is 0.
// Anything not currently published is refused.
func (m *Manifest) ResolvePublishedRevision(policyID int64, revision int) (PolicyRevision, error) {
	var rv PolicyRevision
	var err error
	if revision == 0 {
		rv, err = scanRevision(m.db.QueryRow(`SELECT id, policy_id, revision, status, base_revision, created_at, published_at
			FROM policy_revisions WHERE policy_id = ? AND status = ?
			ORDER BY revision DESC LIMIT 1`, policyID, RevPublished))
		if errors.Is(err, sql.ErrNoRows) {
			return rv, fmt.Errorf("policy %d has no published revision: %w", policyID, ErrRevisionNotPublished)
		}
		return rv, err
	}
	rv, err = m.GetRevision(policyID, revision)
	if err != nil {
		return rv, err
	}
	if rv.Status != RevPublished {
		return rv, fmt.Errorf("policy %d revision %d is %s: %w",
			policyID, revision, rv.Status, ErrRevisionNotPublished)
	}
	return rv, nil
}

// FrozenPolicy is the policy reference recorded on a snapshot at scan start.
type FrozenPolicy struct {
	PolicyID int64
	Revision int
	Rules    []PolicyRule // ordered, frozen into snapshot_policy_rules
}

// SnapshotPolicy returns the frozen policy reference of a snapshot.
func (m *Manifest) SnapshotPolicy(snapshotID int64) (*FrozenPolicy, error) {
	var pid, rev sql.NullInt64
	err := m.db.QueryRow(`SELECT policy_id, policy_revision FROM snapshots WHERE id = ?`,
		snapshotID).Scan(&pid, &rev)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !pid.Valid {
		return nil, nil
	}
	rules, err := m.FrozenRules(snapshotID)
	if err != nil {
		return nil, err
	}
	out := make([]PolicyRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, PolicyRule{Seq: r.Seq, Action: r.Action, Pattern: r.Pattern})
	}
	return &FrozenPolicy{PolicyID: pid.Int64, Revision: int(rev.Int64), Rules: out}, nil
}

// FrozenRule is one rule row frozen onto a snapshot.
type FrozenRule struct {
	Seq     int
	Action  string
	Pattern string
}

// FrozenRules returns the ordered frozen rules of a snapshot.
func (m *Manifest) FrozenRules(snapshotID int64) ([]FrozenRule, error) {
	rows, err := m.db.Query(`SELECT seq, action, pattern FROM snapshot_policy_rules
		WHERE snapshot_id = ? ORDER BY seq`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FrozenRule{}
	for rows.Next() {
		var r FrozenRule
		if err := rows.Scan(&r.Seq, &r.Action, &r.Pattern); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SelectionRecord is one persisted selection-evidence row.
type SelectionRecord struct {
	RelPath     string
	Decision    string
	RuleSeq     int
	RuleAction  string
	RulePattern string
	IsDir       bool
}

// SelectionOf returns the selection evidence of a snapshot, in walk order.
func (m *Manifest) SelectionOf(snapshotID int64) ([]SelectionRecord, error) {
	rows, err := m.db.Query(`SELECT rel_path, decision, rule_seq, rule_action, rule_pattern, is_dir
		FROM snapshot_selection WHERE snapshot_id = ? ORDER BY id`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SelectionRecord{}
	for rows.Next() {
		var r SelectionRecord
		var isDir int
		if err := rows.Scan(&r.RelPath, &r.Decision, &r.RuleSeq, &r.RuleAction, &r.RulePattern, &isDir); err != nil {
			return nil, err
		}
		r.IsDir = isDir != 0
		out = append(out, r)
	}
	return out, rows.Err()
}
