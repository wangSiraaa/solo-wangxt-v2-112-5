package repo

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Policy/revision status values. Policies (lineage) and revisions (immutable
// rule sets) have separate states:
//
//	draft      — revision still editable; it can never be used by a snapshot
//	published  — frozen and usable by snapshots; rules never change
//	retired    — hidden from new snapshots; existing snapshots keep their copy
//
// A policy is "active" while it owns at least one published revision; it can
// still accumulate new drafts. Retiring a policy retires the lineage so new
// snapshots can no longer reference it, but every snapshot created earlier
// keeps its own frozen rule copy.
const (
	RevDraft     = "draft"
	RevPublished = "published"
	RevRetired   = "retired"

	PolicyActive  = "active"
	PolicyRetired = "retired"
)

// ErrPolicyNotFound / ErrRevisionNotFound name missing policy entities.
var (
	ErrPolicyNotFound   = errors.New("policy not found")
	ErrRevisionNotFound = errors.New("policy revision not found")
	ErrNotDraft         = errors.New("revision is not a draft")
	ErrNotPublished     = errors.New("revision is not published")
	ErrPolicyRetired    = errors.New("policy is retired")
)

// Policy is one named, versioned lineage of scan rule sets.
type Policy struct {
	ID          int64
	Name        string
	Status      string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Revision is one immutable rule version. Revision numbers start at 1 and
// increase within a policy.
type Revision struct {
	ID          int64
	PolicyID    int64
	Number      int64
	Status      string
	Comment     string
	CreatedAt   time.Time
	PublishedAt *time.Time
	RulesJSON   []byte // canonical ordered rules, always set
}

const policySchemaSQL = `
CREATE TABLE IF NOT EXISTS policies (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT    NOT NULL UNIQUE,
	status      TEXT    NOT NULL,
	description TEXT    NOT NULL DEFAULT '',
	created_at  TEXT    NOT NULL,
	updated_at  TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS policy_revisions (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	policy_id    INTEGER NOT NULL REFERENCES policies(id),
	number       INTEGER NOT NULL,
	status       TEXT    NOT NULL,
	comment      TEXT    NOT NULL DEFAULT '',
	rules_json   TEXT    NOT NULL DEFAULT '[]',
	created_at   TEXT    NOT NULL,
	published_at TEXT,
	retired_at   TEXT,
	UNIQUE (policy_id, number)
);
CREATE INDEX IF NOT EXISTS idx_rev_policy ON policy_revisions(policy_id, number);

-- What a snapshot froze at scan start. A snapshot references a published
-- revision; the full rule document and rule order are copied here so later
-- retirement/deletion or a new revision cannot change the snapshot's scope.
CREATE TABLE IF NOT EXISTS snapshot_policy_freeze (
	snapshot_id         INTEGER PRIMARY KEY REFERENCES snapshots(id) ON DELETE CASCADE,
	policy_id           INTEGER NOT NULL,
	policy_name         TEXT    NOT NULL,
	revision_id         INTEGER NOT NULL,
	revision_number     INTEGER NOT NULL,
	status_at_freeze    TEXT    NOT NULL,
	frozen_at           TEXT    NOT NULL,
	rules_json          TEXT    NOT NULL
);

-- Selection evidence: every walked path that ended excluded, or that matched
-- more than one rule (e.g. an exclude later overridden by an exception). Each
-- row names the decisive rule; snapshot_selection_hits lists every rule that
-- matched, in rule order, so "why was this file (not) in the manifest?" is
-- answerable years later even if the policy document is gone.
CREATE TABLE IF NOT EXISTS snapshot_selection (
	snapshot_id     INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
	rel_path        TEXT    NOT NULL,
	entry_kind_hint TEXT    NOT NULL DEFAULT '',
	included        INTEGER NOT NULL,
	filter_default  INTEGER NOT NULL DEFAULT 0,
	excluded_by     TEXT    NOT NULL DEFAULT '',
	decisive_order  INTEGER,
	decisive_action TEXT,
	decisive_pattern TEXT,
	PRIMARY KEY (snapshot_id, rel_path)
);

CREATE TABLE IF NOT EXISTS snapshot_selection_hits (
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
	rel_path    TEXT    NOT NULL,
	rule_order  INTEGER NOT NULL,
	action      TEXT    NOT NULL,
	pattern     TEXT    NOT NULL,
	decisive    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (snapshot_id, rel_path, rule_order),
	FOREIGN KEY (snapshot_id, rel_path)
		REFERENCES snapshot_selection(snapshot_id, rel_path) ON DELETE CASCADE
);
`

// CreatePolicy inserts a new lineage and its first draft revision in one
// transaction. rulesJSON must already have passed lexical validation.
func (m *Manifest) CreatePolicy(name, description string, rulesJSON []byte, comment string) (policyID, revisionID int64, err error) {
	tx, err := m.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.Exec(`INSERT INTO policies (name, status, description, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, name, PolicyActive, description, now, now)
	if err != nil {
		return 0, 0, fmt.Errorf("create policy: %w", err)
	}
	policyID, _ = res.LastInsertId()
	revisionID, err = insertRevision(tx, policyID, 1, RevDraft, comment, rulesJSON, nil)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return policyID, revisionID, nil
}

// AddDraftRevision appends a new editable revision to an existing policy. The
// source revision may be nil (empty rules) or any revision's copied rule set;
// revisions are never linked, so changing the new draft never touches the
// source. This is the only legal way to "edit" a published revision: copy.
func (m *Manifest) AddDraftRevision(policyID int64, comment string, rulesJSON []byte) (revisionID, number int64, err error) {
	tx, err := m.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	var status string
	if qerr := tx.QueryRow(`SELECT status FROM policies WHERE id = ?`, policyID).Scan(&status); qerr != nil {
		if errors.Is(qerr, sql.ErrNoRows) {
			return 0, 0, ErrPolicyNotFound
		}
		return 0, 0, qerr
	}
	if status == PolicyRetired {
		return 0, 0, ErrPolicyRetired
	}
	var maxNo sql.NullInt64
	if qerr := tx.QueryRow(`SELECT max(number) FROM policy_revisions WHERE policy_id = ?`,
		policyID).Scan(&maxNo); qerr != nil {
		return 0, 0, qerr
	}
	number = maxNo.Int64 + 1
	revisionID, err = insertRevision(tx, policyID, number, RevDraft, comment, rulesJSON, nil)
	if err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(`UPDATE policies SET updated_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), policyID); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return revisionID, number, nil
}

func insertRevision(tx *sql.Tx, policyID, number int64, status, comment string,
	rulesJSON []byte, publishedAt *string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.Exec(`INSERT INTO policy_revisions
		(policy_id, number, status, comment, rules_json, created_at, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		policyID, number, status, comment, string(rulesJSON), now, publishedAt)
	if err != nil {
		return 0, fmt.Errorf("insert revision: %w", err)
	}
	return res.LastInsertId()
}

// UpdateDraftRules replaces the rule set of a draft revision. Published or
// retired revisions are immutable and rejected.
func (m *Manifest) UpdateDraftRules(revisionID int64, comment string, rulesJSON []byte) error {
	res, err := m.db.Exec(`UPDATE policy_revisions SET rules_json = ?, comment = ?
		WHERE id = ? AND status = ?`,
		string(rulesJSON), comment, revisionID, RevDraft)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// distinguish missing from non-draft for the caller's error message
		var status string
		qerr := m.db.QueryRow(`SELECT status FROM policy_revisions WHERE id = ?`,
			revisionID).Scan(&status)
		if errors.Is(qerr, sql.ErrNoRows) {
			return ErrRevisionNotFound
		}
		if qerr != nil {
			return qerr
		}
		return fmt.Errorf("%w: revision %d is %s", ErrNotDraft, revisionID, status)
	}
	_, err = m.db.Exec(`UPDATE policies SET updated_at = ? WHERE id =
		(SELECT policy_id FROM policy_revisions WHERE id = ?)`,
		time.Now().UTC().Format(time.RFC3339Nano), revisionID)
	return err
}

// PublishRevision flips draft -> published. The status guard in the UPDATE is
// the concurrency gate: two windows publishing the same draft concurrently
// both run
//
//	UPDATE ... SET status='published' WHERE id=? AND status='draft'
//
// under SQLite's immediate write transaction, so exactly one affects a row.
// The loser re-reads and sees the published revision (or a retirement) and
// gets an error rather than a silent no-op.
func (m *Manifest) PublishRevision(revisionID int64) (*Revision, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rev, err := scanRevision(tx.QueryRow(`SELECT `+revisionCols+`
		FROM policy_revisions WHERE id = ?`, revisionID))
	if err != nil {
		return nil, err
	}
	if rev.Status == RevRetired {
		return nil, fmt.Errorf("%w: revision %d is retired", ErrNotDraft, revisionID)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.Exec(`UPDATE policy_revisions
		SET status = ?, published_at = COALESCE(published_at, ?)
		WHERE id = ? AND status = ?`,
		RevPublished, now, revisionID, RevDraft)
	if err != nil {
		return nil, fmt.Errorf("publish revision: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		after, aerr := scanRevision(tx.QueryRow(`SELECT `+revisionCols+`
			FROM policy_revisions WHERE id = ?`, revisionID))
		if aerr != nil {
			return nil, aerr
		}
		if after.Status == RevPublished {
			return nil, fmt.Errorf("%w: revision %d was already published by a concurrent request",
				ErrNotDraft, revisionID)
		}
		return nil, fmt.Errorf("%w: revision %d is %s", ErrNotDraft, revisionID, after.Status)
	}
	if _, err := tx.Exec(`UPDATE policies SET updated_at = ? WHERE id = ?`,
		now, rev.PolicyID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	rev2, err := m.GetRevision(revisionID)
	if err != nil {
		return nil, err
	}
	return &rev2, nil
}

// RetirePolicy retires a lineage: its published revision becomes "retired"
// and drafts are retired too, so no new snapshot may select it. Previously
// created snapshots keep their frozen rule copy untouched.
func (m *Manifest) RetirePolicy(policyID int64) error {
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE policies SET status = ?, updated_at = ? WHERE id = ?`,
		PolicyRetired, time.Now().UTC().Format(time.RFC3339Nano), policyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPolicyNotFound
	}
	if _, err := tx.Exec(`UPDATE policy_revisions SET status = ?, retired_at = ?
		WHERE policy_id = ? AND status IN (?, ?)`,
		RevRetired, time.Now().UTC().Format(time.RFC3339Nano), policyID, RevPublished, RevDraft); err != nil {
		return err
	}
	return tx.Commit()
}

const revisionCols = `id, policy_id, number, status, comment, rules_json, created_at, published_at`

func scanRevision(row interface{ Scan(...any) error }) (Revision, error) {
	var r Revision
	var created string
	var published sql.NullString
	var rules string
	if err := row.Scan(&r.ID, &r.PolicyID, &r.Number, &r.Status, &r.Comment,
		&rules, &created, &published); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrRevisionNotFound
		}
		return r, err
	}
	r.RulesJSON = []byte(rules)
	r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if published.Valid {
		t, _ := time.Parse(time.RFC3339Nano, published.String)
		r.PublishedAt = &t
	}
	return r, nil
}

// GetRevision fetches one revision.
func (m *Manifest) GetRevision(id int64) (Revision, error) {
	return scanRevision(m.db.QueryRow(`SELECT `+revisionCols+`
		FROM policy_revisions WHERE id = ?`, id))
}

// GetRevisionForSnapshot validates that revisionID is published and returns
// it together with its owning policy. Called inside the snapshot-freeze
// transaction: the revision's status is read with an immediate write lock, so
// a concurrent retirement racing the snapshot cannot change the frozen scope.
func (m *Manifest) getPublishedRevisionLocked(tx *sql.Tx, revisionID int64) (Policy, Revision, error) {
	var p Policy
	var pcreated, pupdated string
	err := tx.QueryRow(`SELECT id, name, status, description, created_at, updated_at
		FROM policies WHERE id = (SELECT policy_id FROM policy_revisions WHERE id = ?)`,
		revisionID).Scan(&p.ID, &p.Name, &p.Status, &p.Description, &pcreated, &pupdated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, Revision{}, ErrRevisionNotFound
	}
	if err != nil {
		return p, Revision{}, err
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, pcreated)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, pupdated)
	if p.Status != PolicyActive {
		return p, Revision{}, ErrPolicyRetired
	}
	r, err := scanRevision(tx.QueryRow(`SELECT `+revisionCols+`
		FROM policy_revisions WHERE id = ? AND status = ?`, revisionID, RevPublished))
	if err != nil {
		if errors.Is(err, ErrRevisionNotFound) {
			return p, r, ErrNotPublished
		}
		return p, r, err
	}
	return p, r, nil
}

// ListPolicies returns all lineages, newest first.
func (m *Manifest) ListPolicies() ([]Policy, error) {
	rows, err := m.db.Query(`SELECT id, name, status, description, created_at, updated_at
		FROM policies ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		var p Policy
		var created, updated string
		if err := rows.Scan(&p.ID, &p.Name, &p.Status, &p.Description, &created, &updated); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPolicy fetches one lineage.
func (m *Manifest) GetPolicy(id int64) (Policy, error) {
	var p Policy
	var created, updated string
	err := m.db.QueryRow(`SELECT id, name, status, description, created_at, updated_at
		FROM policies WHERE id = ?`, id).Scan(
		&p.ID, &p.Name, &p.Status, &p.Description, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrPolicyNotFound
	}
	if err != nil {
		return p, err
	}
	p.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	p.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return p, nil
}

// ListRevisions returns all revisions of a policy by ascending number.
func (m *Manifest) ListRevisions(policyID int64) ([]Revision, error) {
	rows, err := m.db.Query(`SELECT `+revisionCols+`
		FROM policy_revisions WHERE policy_id = ? ORDER BY number`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		r, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
