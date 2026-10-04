package backup

import (
	"fmt"
	"path"
	"strings"
)

// Scan policies decide, per slash-relative path, whether an entry belongs to
// a snapshot. Matching is purely lexical: patterns are glob-matched against
// the slash-separated path relative to the snapshot root. Nothing is ever
// resolved through the filesystem — no ".." evaluation, no absolute paths, no
// symlink following — so a rule can never select anything outside the root.
//
// Semantics:
//   - Rules are evaluated in stored order; the last matching rule wins.
//   - Action "include" admits the path, "exclude" drops it, "exception"
//     re-admits a path an earlier exclude rule removed.
//   - When the policy has no include rules, everything is included by
//     default; with at least one include rule the default flips to exclude.
//   - A pattern without "/" matches the basename at any depth ("*.tmp");
//     a pattern with "/" is anchored to the root ("docs/*.tmp").
//   - "**" matches any number of path segments (including zero); a trailing
//     "/" restricts the rule to directories.
//   - Excluding a directory prunes the whole subtree; an exception below a
//     pruned directory is never reached (same rule as gitignore).

// Rule actions stored in policy revisions.
const (
	ActionInclude   = "include"
	ActionExclude   = "exclude"
	ActionException = "exception"
)

// PolicyRule is one ordered rule of a policy revision.
type PolicyRule struct {
	Seq     int    `json:"seq"`
	Action  string `json:"action"`
	Pattern string `json:"pattern"`
}

// SelectionEvidence explains one rule-driven decision about a path. Excluded
// paths are recorded with the rule that dropped them; exception records show
// which exception rule kept a path that an exclude rule also matched, so a
// manifest can later justify both absences and surprising presences.
type SelectionEvidence struct {
	RelPath     string `json:"rel_path"`
	Decision    string `json:"decision"` // "excluded" | "exception"
	RuleSeq     int    `json:"rule_seq"`
	RuleAction  string `json:"rule_action"`
	RulePattern string `json:"rule_pattern"`
	IsDir       bool   `json:"is_dir"`
}

// ValidateRulePattern rejects patterns that are not purely lexical and
// root-contained: absolute patterns, ".." or "." segments, empty segments,
// backslashes, malformed globs, and any pattern that would match the
// snapshot root itself (the root must always be scanned).
func ValidateRulePattern(pattern string) error {
	if pattern == "" {
		return fmt.Errorf("empty pattern")
	}
	if strings.ContainsRune(pattern, '\\') {
		return fmt.Errorf("pattern %q: backslash is not supported, use '/'", pattern)
	}
	if strings.HasPrefix(pattern, "/") {
		return fmt.Errorf("pattern %q: absolute patterns are not allowed, rules are relative to the snapshot root", pattern)
	}
	body := strings.TrimSuffix(pattern, "/")
	if body == "" {
		return fmt.Errorf("pattern %q: matches only the snapshot root", pattern)
	}
	segs := strings.Split(body, "/")
	for _, seg := range segs {
		switch seg {
		case "":
			return fmt.Errorf("pattern %q: empty path segment", pattern)
		case ".":
			return fmt.Errorf("pattern %q: '.' segments are not allowed", pattern)
		case "..":
			return fmt.Errorf("pattern %q: '..' segments are not allowed, rules must not escape the snapshot root", pattern)
		case "**":
			// valid multi-segment wildcard
		default:
			if _, err := path.Match(seg, ""); err != nil {
				return fmt.Errorf("pattern %q: invalid glob segment %q: %v", pattern, seg, err)
			}
		}
	}
	// The root entry (rel path ".") is always scanned; a rule matching it
	// conflicts with the root itself.
	if matchSegments(segs, []string{"."}) {
		return fmt.Errorf("pattern %q: rule conflicts with the snapshot root (the root is always included)", pattern)
	}
	return nil
}

// ValidateRules checks action names and patterns of an ordered rule set.
func ValidateRules(rules []PolicyRule) error {
	for i, r := range rules {
		switch r.Action {
		case ActionInclude, ActionExclude, ActionException:
		default:
			return fmt.Errorf("rule %d: unknown action %q (want include|exclude|exception)", i+1, r.Action)
		}
		if err := ValidateRulePattern(r.Pattern); err != nil {
			return fmt.Errorf("rule %d (%s %q): %v", i+1, r.Action, r.Pattern, err)
		}
	}
	return nil
}

// CompiledPolicy is a validated, immutable rule set ready for evaluation.
type CompiledPolicy struct {
	Rules      []PolicyRule // as persisted, in order
	compiled   []compiledRule
	hasInclude bool
}

type compiledRule struct {
	action   string
	segs     []string
	dirOnly  bool
	anchored bool // pattern contains '/': match full path, else basename
}

// CompilePolicy validates rules and prepares them for matching.
func CompilePolicy(rules []PolicyRule) (*CompiledPolicy, error) {
	if err := ValidateRules(rules); err != nil {
		return nil, err
	}
	p := &CompiledPolicy{Rules: append([]PolicyRule(nil), rules...)}
	for _, r := range rules {
		body := strings.TrimSuffix(r.Pattern, "/")
		cr := compiledRule{
			action:   r.Action,
			segs:     strings.Split(body, "/"),
			dirOnly:  strings.HasSuffix(r.Pattern, "/"),
			anchored: strings.Contains(body, "/"),
		}
		p.compiled = append(p.compiled, cr)
		if r.Action == ActionInclude {
			p.hasInclude = true
		}
	}
	return p, nil
}

// matches reports whether the rule selects rel (slash-relative, never ".").
func (c *compiledRule) matches(rel string, isDir bool) bool {
	if c.dirOnly && !isDir {
		return false
	}
	if !c.anchored {
		// Basename rule: matches at any depth.
		base := rel
		if i := strings.LastIndexByte(rel, '/'); i >= 0 {
			base = rel[i+1:]
		}
		if c.segs[0] == "**" {
			return true
		}
		ok, err := path.Match(c.segs[0], base)
		return err == nil && ok
	}
	return matchSegments(c.segs, strings.Split(rel, "/"))
}

// matchSegments matches glob pattern segments against path segments; "**"
// consumes zero or more segments.
func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// Decide evaluates the ordered rules for one path. It returns whether the
// path is included and, when a rule materially decided the outcome, the
// evidence record for it: every exclusion, and every exception that kept a
// path an exclude rule had matched.
func (p *CompiledPolicy) Decide(rel string, isDir bool) (included bool, ev *SelectionEvidence) {
	included = !p.hasInclude
	var deciding = -1
	lastExclude := -1
	for i := range p.compiled {
		if !p.compiled[i].matches(rel, isDir) {
			continue
		}
		switch p.compiled[i].action {
		case ActionExclude:
			included, deciding, lastExclude = false, i, i
		case ActionInclude, ActionException:
			included, deciding = true, i
		}
	}
	if deciding < 0 {
		return included, nil
	}
	rule := p.Rules[deciding]
	switch {
	case !included:
		return false, &SelectionEvidence{
			RelPath: rel, Decision: "excluded",
			RuleSeq: rule.Seq, RuleAction: rule.Action, RulePattern: rule.Pattern,
			IsDir: isDir,
		}
	case p.compiled[deciding].action == ActionException && lastExclude >= 0:
		// Kept only because an exception overrode an exclude: record it so
		// the manifest can explain why this path is present.
		return true, &SelectionEvidence{
			RelPath: rel, Decision: "exception",
			RuleSeq: rule.Seq, RuleAction: rule.Action, RulePattern: rule.Pattern,
			IsDir: isDir,
		}
	}
	return included, nil
}
