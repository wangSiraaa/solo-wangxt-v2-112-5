package repo

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SnapshotFreeze is the immutable copy of a published revision attached to a
// snapshot at scan start.
type SnapshotFreeze struct {
	SnapshotID     int64
	PolicyID       int64
	PolicyName     string
	RevisionID     int64
	RevisionNumber int64
	StatusAtFreeze string
	FrozenAt       time.Time
	RulesJSON      []byte
}

// SelectionHit is one rule that matched a walked path.
type SelectionHit struct {
	Order    int    `json:"order"`
	Action   string `json:"action"`
	Pattern  string `json:"pattern"`
	Decisive bool   `json:"decisive"`
}

// SelectionRecord explains what happened to one walked path under the frozen
// revision.
type SelectionRecord struct {
	RelPath       string `json:"rel_path"`
	KindHint      string `json:"kind_hint"`
	Included      bool   `json:"included"`
	FilterDefault bool   `json:"filter_default,omitempty"`
	// ExcludedBy is set when the path itself matched no excluding rule but is
	// outside scope because an ancestor directory was excluded. It names that
	// ancestor (slash-relative); the ancestor's decisive rule is in Hits.
	ExcludedBy      string         `json:"excluded_by,omitempty"`
	DecisiveOrder   int            `json:"decisive_order,omitempty"`
	DecisiveAction  string         `json:"decisive_action,omitempty"`
	DecisivePattern string         `json:"decisive_pattern,omitempty"`
	Hits            []SelectionHit `json:"hits,omitempty"`
}

// BeginSnapshotWithPolicy atomically creates the pending snapshot row and
// freezes a published revision into snapshot_policy_freeze. This is the
// "freeze at scan start" step: it runs in one immediate transaction, so a
// concurrent publish/retire cannot change the scope once scanning begins. A
// draft or retired revision is refused and no snapshot row is created.
func (m *Manifest) BeginSnapshotWithPolicy(root string, polynomial uint64,
	message string, revisionID int64) (int64, *SnapshotFreeze, error) {
	tx, err := m.db.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback()

	pol, rev, err := m.getPublishedRevisionLocked(tx, revisionID)
	if err != nil {
		return 0, nil, err
	}
	res, err := tx.Exec(`INSERT INTO snapshots
		(root_path, status, polynomial, created_at, message)
		VALUES (?, ?, ?, ?, ?)`,
		root, StatusPending, int64(polynomial),
		time.Now().UTC().Format(time.RFC3339Nano), message)
	if err != nil {
		return 0, nil, fmt.Errorf("begin snapshot: %w", err)
	}
	id, _ := res.LastInsertId()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(`INSERT INTO snapshot_policy_freeze
		(snapshot_id, policy_id, policy_name, revision_id, revision_number,
		 status_at_freeze, frozen_at, rules_json)
		VALUES (?,?,?,?,?,?,?,?)`,
		id, pol.ID, pol.Name, rev.ID, rev.Number, rev.Status, now, string(rev.RulesJSON)); err != nil {
		return 0, nil, fmt.Errorf("freeze revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, err
	}
	frozenAt, _ := time.Parse(time.RFC3339Nano, now)
	return id, &SnapshotFreeze{
		SnapshotID:     id,
		PolicyID:       pol.ID,
		PolicyName:     pol.Name,
		RevisionID:     rev.ID,
		RevisionNumber: rev.Number,
		StatusAtFreeze: rev.Status,
		FrozenAt:       frozenAt,
		RulesJSON:      append([]byte(nil), rev.RulesJSON...),
	}, nil
}

// GetFreeze returns the frozen revision of a snapshot, or nil (no error) when
// the snapshot used the full-scan default.
func (m *Manifest) GetFreeze(snapshotID int64) (*SnapshotFreeze, error) {
	var f SnapshotFreeze
	var frozenAt, rules string
	err := m.db.QueryRow(`SELECT snapshot_id, policy_id, policy_name, revision_id,
		revision_number, status_at_freeze, frozen_at, rules_json
		FROM snapshot_policy_freeze WHERE snapshot_id = ?`, snapshotID).Scan(
		&f.SnapshotID, &f.PolicyID, &f.PolicyName, &f.RevisionID,
		&f.RevisionNumber, &f.StatusAtFreeze, &frozenAt, &rules)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.FrozenAt, _ = time.Parse(time.RFC3339Nano, frozenAt)
	f.RulesJSON = []byte(rules)
	return &f, nil
}

// SaveSelectionEvidence persists per-path selection decisions in one
// transaction. It is written after the walk even when the scan itself failed,
// so an interrupted policy scan still leaves a locatable explanation.
func (m *Manifest) SaveSelectionEvidence(snapshotID int64, recs []SelectionRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := m.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range recs {
		var decisiveOrder sql.NullInt64
		var decisiveAction, decisivePattern sql.NullString
		if r.DecisiveAction != "" {
			decisiveOrder = sql.NullInt64{Int64: int64(r.DecisiveOrder), Valid: true}
			decisiveAction = sql.NullString{String: r.DecisiveAction, Valid: true}
			decisivePattern = sql.NullString{String: r.DecisivePattern, Valid: true}
		}
		if _, err := tx.Exec(`INSERT INTO snapshot_selection
			(snapshot_id, rel_path, entry_kind_hint, included, filter_default,
			 excluded_by, decisive_order, decisive_action, decisive_pattern)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			snapshotID, r.RelPath, r.KindHint, boolInt(r.Included), boolInt(r.FilterDefault),
			r.ExcludedBy, decisiveOrder, decisiveAction, decisivePattern); err != nil {
			return fmt.Errorf("save selection %q: %w", r.RelPath, err)
		}
		for _, h := range r.Hits {
			if _, err := tx.Exec(`INSERT INTO snapshot_selection_hits
				(snapshot_id, rel_path, rule_order, action, pattern, decisive)
				VALUES (?,?,?,?,?,?)`,
				snapshotID, r.RelPath, h.Order, h.Action, h.Pattern, boolInt(h.Decisive)); err != nil {
				return fmt.Errorf("save selection hit %q: %w", r.RelPath, err)
			}
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ListSelection returns the persisted selection evidence of a snapshot,
// ordered by path.
func (m *Manifest) ListSelection(snapshotID int64) ([]SelectionRecord, error) {
	rows, err := m.db.Query(`SELECT rel_path, entry_kind_hint, included, filter_default,
		excluded_by, decisive_order, decisive_action, decisive_pattern
		FROM snapshot_selection WHERE snapshot_id = ? ORDER BY rel_path`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SelectionRecord
	for rows.Next() {
		var r SelectionRecord
		var kind string
		var included, filter int
		var excludedBy string
		var order sql.NullInt64
		var action, pattern sql.NullString
		if err := rows.Scan(&r.RelPath, &kind, &included, &filter, &excludedBy,
			&order, &action, &pattern); err != nil {
			return nil, err
		}
		r.KindHint = kind
		r.Included = included != 0
		r.FilterDefault = filter != 0
		r.ExcludedBy = excludedBy
		if action.Valid {
			r.DecisiveOrder = int(order.Int64)
			r.DecisiveAction = action.String
			r.DecisivePattern = pattern.String
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	hrows, err := m.db.Query(`SELECT rel_path, rule_order, action, pattern, decisive
		FROM snapshot_selection_hits WHERE snapshot_id = ?
		ORDER BY rel_path, rule_order`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer hrows.Close()
	byPath := map[string]*SelectionRecord{}
	for i := range out {
		byPath[out[i].RelPath] = &out[i]
	}
	for hrows.Next() {
		var rel, action, pattern string
		var order, decisive int
		if err := hrows.Scan(&rel, &order, &action, &pattern, &decisive); err != nil {
			return nil, err
		}
		if r := byPath[rel]; r != nil {
			r.Hits = append(r.Hits, SelectionHit{
				Order: order, Action: action, Pattern: pattern, Decisive: decisive != 0,
			})
		}
	}
	return out, hrows.Err()
}
