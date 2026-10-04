package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"incbackup/internal/policy"
	"incbackup/internal/repo"
)

// rulesDocument is the canonical JSON shape frozen into revisions.
func encodeRules(rules []policy.Rule) ([]byte, error) {
	for i := range rules {
		rules[i].Order = i // position is authoritative
	}
	return json.Marshal(rules)
}

// CreatePolicy validates rules, creates the lineage and its first draft.
func (e *Engine) CreatePolicy(name, description, comment string, rules []policy.Rule) (policyID, revisionID int64, err error) {
	compiled, err := policy.Compile(rules)
	if err != nil {
		return 0, 0, err
	}
	doc, err := encodeRules(compiled.Rules())
	if err != nil {
		return 0, 0, err
	}
	return e.Manifest.CreatePolicy(name, description, doc, comment)
}

// CopyRevision starts a new draft from an existing revision of any status:
// published revisions can never be edited, so changing anything means copying
// the rules into a fresh version.
func (e *Engine) CopyRevision(sourceRevisionID int64, comment string) (newRevisionID, number int64, rules []policy.Rule, err error) {
	src, gerr := e.Manifest.GetRevision(sourceRevisionID)
	if gerr != nil {
		return 0, 0, nil, gerr
	}
	var got []policy.Rule
	if uerr := json.Unmarshal(src.RulesJSON, &got); uerr != nil {
		return 0, 0, nil, fmt.Errorf("source revision unreadable: %w", uerr)
	}
	newID, num, aerr := e.Manifest.AddDraftRevision(src.PolicyID, comment, src.RulesJSON)
	return newID, num, got, aerr
}

// UpdateDraft validates and replaces a draft revision's rules.
func (e *Engine) UpdateDraft(revisionID int64, comment string, rules []policy.Rule) error {
	compiled, err := policy.Compile(rules)
	if err != nil {
		return err
	}
	doc, err := encodeRules(compiled.Rules())
	if err != nil {
		return err
	}
	return e.Manifest.UpdateDraftRules(revisionID, comment, doc)
}

// PublishPolicyRevision publishes a draft. Two windows racing to publish the
// same policy: the repo's one-published partial index accepts exactly one
// revision and rejects the other without changing it.
func (e *Engine) PublishPolicyRevision(revisionID int64) (*repo.Revision, error) {
	return e.Manifest.PublishRevision(revisionID)
}

// RetirePolicy deactivates a lineage.
func (e *Engine) RetirePolicy(policyID int64) error {
	return e.Manifest.RetirePolicy(policyID)
}

// PreviewItem is one previewed path with its decision and matched rules.
type PreviewItem struct {
	RelPath       string             `json:"rel_path"`
	KindHint      string             `json:"kind_hint"`
	Included      bool               `json:"included"`
	FilterDefault bool               `json:"filter_default,omitempty"`
	ExcludedBy    string             `json:"excluded_by,omitempty"`
	Decisive      *policy.Rule       `json:"decisive_rule,omitempty"`
	Hits          []policy.RuleMatch `json:"hits,omitempty"`
}

// Preview walks root with the given compiled revision without storing
// anything, returning the decision for every path plus counts. It never
// follows symlinks and applies the same lexical containment and ancestor
// exclusion as a real scan. The revision must be a draft or published
// document the caller already holds; previews do not freeze anything.
func (e *Engine) Preview(root string, rules []policy.Rule) ([]PreviewItem, int, int, error) {
	compiled, err := policy.Compile(rules)
	if err != nil {
		return nil, 0, 0, err
	}
	root, err = absRoot(root)
	if err != nil {
		return nil, 0, 0, err
	}
	walker := policy.NewWalker(compiled)
	var items []PreviewItem
	included, excluded := 0, 0
	werr := walk(root, func(rel string, d dirInfo) {
		if rel == "." {
			return
		}
		dec := walker.Visit(rel, d.isDir)
		it := PreviewItem{
			RelPath:       rel,
			KindHint:      d.kind,
			Included:      dec.Included,
			FilterDefault: dec.FilterDefault,
			ExcludedBy:    dec.ExcludedBy,
			Hits:          dec.Matches,
		}
		if len(dec.Matches) > 0 {
			for i := range dec.Matches {
				if dec.Matches[i].Decisive {
					r := dec.Matches[i].Rule
					it.Decisive = &r
				}
			}
		}
		if dec.Included {
			included++
		} else {
			excluded++
		}
		items = append(items, it)
	})
	if werr != nil {
		return nil, 0, 0, werr
	}
	sort.Slice(items, func(i, j int) bool { return items[i].RelPath < items[j].RelPath })
	return items, included, excluded, nil
}

// ErrPolicyConflict marks a rejected publish/update due to lifecycle state.
var ErrPolicyConflict = errors.New("policy lifecycle conflict")

// IsPolicyNotFound reports missing policy entities.
func IsPolicyNotFound(err error) bool {
	return errors.Is(err, repo.ErrPolicyNotFound) || errors.Is(err, repo.ErrRevisionNotFound)
}

type dirInfo struct {
	isDir bool
	kind  string
}

func absRoot(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("snapshot root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("snapshot root %q is not a directory", root)
	}
	return root, nil
}

// walk enumerates root without following symlinks (readdir dirent types,
// Lstat) and invokes fn with the slash-relative path. Preview-only: nothing is
// read or stored.
func walk(root string, fn func(rel string, d dirInfo)) error {
	return filepath.WalkDir(root, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree is simply absent from a preview
		}
		info, err := os.Lstat(p)
		if err != nil {
			return nil
		}
		rel := relPath(root, p)
		fn(rel, dirInfo{isDir: info.IsDir(), kind: kindHint(info)})
		return nil
	})
}
