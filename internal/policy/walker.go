package policy

import "strings"

// Walker applies a Compiled revision along a depth-first directory walk and
// enforces ancestor exclusion: once a directory is excluded, every descendant
// is excluded regardless of its own rules (an "include" or "exception"
// cannot resurrect files through an excluded directory). The evidence for an
// inherited exclusion names the ancestor directory that closed the subtree.
//
// It is stateful on purpose: filepath.WalkDir visits parents before children,
// which is exactly the discipline needed here. One Walker per walk.
type Walker struct {
	c *Compiled
	// excludedAncestor[rel] = the nearest excluded ancestor dir (may be rel
	// itself for an excluded directory).
	excludedAncestor map[string]string
}

// NewWalker wraps a compiled revision.
func NewWalker(c *Compiled) *Walker {
	return &Walker{c: c, excludedAncestor: map[string]string{}}
}

// Visit evaluates one walked path. For excluded directories the ancestor set
// is updated so subsequent child visits inherit the exclusion.
func (w *Walker) Visit(rel string, isDir bool) Decision {
	if rel == "" || rel == "." {
		return Decision{Included: true}
	}
	if anc := w.nearestExcludedAncestor(rel); anc != "" {
		d := w.inherited(rel, anc)
		if isDir {
			w.excludedAncestor[rel] = anc
		}
		return d
	}
	d := w.c.Decide(rel, isDir)
	if !d.Included && isDir {
		w.excludedAncestor[rel] = rel
	}
	return d
}

// nearestExcludedAncestor returns rel's nearest excluded ancestor directory
// (walking up through the recorded excluded dirs), or "".
func (w *Walker) nearestExcludedAncestor(rel string) string {
	for {
		i := strings.LastIndexByte(rel, '/')
		if i < 0 {
			return ""
		}
		rel = rel[:i]
		if anc, ok := w.excludedAncestor[rel]; ok {
			return anc
		}
	}
}

// inherited builds the decision for a path whose subtree was closed by an
// excluded ancestor. The ancestor's own decisive rule is copied into the
// hits so evidence says precisely which rule is responsible.
func (w *Walker) inherited(rel, anc string) Decision {
	// Re-evaluate the ancestor to recover its decisive rule. The ancestor map
	// guarantees the ancestor's decision was excluded.
	ad := w.c.Decide(anc, true)
	rule := decisiveRuleOf(ad)
	d := Decision{Included: false, ExcludedBy: anc}
	if rule != nil {
		d.Matches = []RuleMatch{{Rule: *rule, Decisive: true}}
	}
	return d
}

func decisiveRuleOf(d Decision) *Rule {
	for i := range d.Matches {
		if d.Matches[i].Decisive {
			r := d.Matches[i].Rule
			return &r
		}
	}
	return nil
}
