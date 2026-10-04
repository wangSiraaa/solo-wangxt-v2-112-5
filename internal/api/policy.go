package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"incbackup/internal/backup"
	"incbackup/internal/repo"
)

// ---- request/response shapes ----

type ruleReq struct {
	Action  string `json:"action"`
	Pattern string `json:"pattern"`
}

type policyResp struct {
	ID        int64          `json:"id"`
	Name      string         `json:"name"`
	CreatedAt time.Time      `json:"created_at"`
	Revisions []revisionResp `json:"revisions,omitempty"`
}

type revisionResp struct {
	ID           int64      `json:"id"`
	Revision     int        `json:"revision"`
	Status       string     `json:"status"`
	BaseRevision int        `json:"base_revision"`
	Rules        []ruleResp `json:"rules"`
	CreatedAt    time.Time  `json:"created_at"`
	PublishedAt  *time.Time `json:"published_at,omitempty"`
}

type ruleResp struct {
	Seq     int    `json:"seq"`
	Action  string `json:"action"`
	Pattern string `json:"pattern"`
}

func toRuleResp(rules []repo.PolicyRule) []ruleResp {
	out := make([]ruleResp, 0, len(rules))
	for _, r := range rules {
		out = append(out, ruleResp{Seq: r.Seq, Action: r.Action, Pattern: r.Pattern})
	}
	return out
}

func toRevisionResp(rv repo.PolicyRevision, rules []repo.PolicyRule) revisionResp {
	return revisionResp{
		ID:           rv.ID,
		Revision:     rv.Revision,
		Status:       rv.Status,
		BaseRevision: rv.BaseRevision,
		Rules:        toRuleResp(rules),
		CreatedAt:    rv.CreatedAt,
		PublishedAt:  rv.PublishedAt,
	}
}

// rulesFromReq converts request rules into an ordered, validated rule set.
// Validation errors are reported per rule so the caller can fix them all at
// once; anything illegal means the whole set is refused.
func rulesFromReq(list []ruleReq) ([]repo.PolicyRule, []string) {
	rules := make([]repo.PolicyRule, 0, len(list))
	var problems []string
	for i, r := range list {
		rules = append(rules, repo.PolicyRule{Seq: i + 1, Action: r.Action, Pattern: r.Pattern})
	}
	backupRules := make([]backup.PolicyRule, 0, len(rules))
	for _, r := range rules {
		backupRules = append(backupRules, backup.PolicyRule{Seq: r.Seq, Action: r.Action, Pattern: r.Pattern})
	}
	if err := backup.ValidateRules(backupRules); err != nil {
		problems = append(problems, err.Error())
	}
	return rules, problems
}

func parsePolicyIDs(w http.ResponseWriter, r *http.Request) (policyID int64, revision int, ok bool) {
	policyID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_id", "policy id must be an integer", nil)
		return 0, 0, false
	}
	if rv := r.PathValue("rev"); rv != "" {
		n, err := strconv.Atoi(rv)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "bad_revision", "revision must be a positive integer", nil)
			return 0, 0, false
		}
		revision = n
	}
	return policyID, revision, true
}

// ---- handlers ----

type createPolicyReq struct {
	Name    string    `json:"name"`
	Rules   []ruleReq `json:"rules"`
	Publish bool      `json:"publish"` // validate + publish immediately
}

func (s *Server) createPolicy(w http.ResponseWriter, r *http.Request) {
	var req createPolicyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name is required", nil)
		return
	}
	rules, problems := rulesFromReq(req.Rules)
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":   "invalid_rules",
			"message": "policy rules are not legal lexical patterns",
			"details": problems,
		})
		return
	}
	policyID, _, err := s.Engine.Manifest.CreatePolicy(req.Name, rules)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, http.StatusConflict, "policy_exists", err.Error(), nil)
			return
		}
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	if req.Publish {
		if err := s.Engine.Manifest.PublishRevision(policyID, 1); err != nil {
			writeErr(w, http.StatusInternalServerError, "publish_failed", err.Error(), nil)
			return
		}
	}
	s.writePolicy(w, policyID, http.StatusCreated)
}

func (s *Server) listPolicies(w http.ResponseWriter, r *http.Request) {
	policies, revs, rules, err := s.Engine.Manifest.ListPolicies()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	out := make([]policyResp, 0, len(policies))
	for _, p := range policies {
		pr := policyResp{ID: p.ID, Name: p.Name, CreatedAt: p.CreatedAt}
		for _, rv := range revs[p.ID] {
			pr.Revisions = append(pr.Revisions, toRevisionResp(rv, rules[rv.ID]))
		}
		out = append(out, pr)
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	policyID, _, ok := parsePolicyIDs(w, r)
	if !ok {
		return
	}
	s.writePolicy(w, policyID, http.StatusOK)
}

// writePolicy renders one policy with all its revisions and rules.
func (s *Server) writePolicy(w http.ResponseWriter, policyID int64, status int) {
	p, err := s.Engine.Manifest.GetPolicy(policyID)
	if errors.Is(err, repo.ErrPolicyNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "policy does not exist", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	revs, err := s.Engine.Manifest.RevisionsOf(policyID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	pr := policyResp{ID: p.ID, Name: p.Name, CreatedAt: p.CreatedAt}
	for _, rv := range revs {
		rules, err := s.Engine.Manifest.RulesOfRevision(rv.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
			return
		}
		pr.Revisions = append(pr.Revisions, toRevisionResp(rv, rules))
	}
	writeJSON(w, status, map[string]any{"policy": pr})
}

type copyRevisionReq struct {
	FromRevision int       `json:"from_revision"` // 0 = latest published
	Rules        []ruleReq `json:"rules"`         // optional: replace copied rules at once
}

func (s *Server) copyRevision(w http.ResponseWriter, r *http.Request) {
	policyID, _, ok := parsePolicyIDs(w, r)
	if !ok {
		return
	}
	var req copyRevisionReq
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
			return
		}
	}
	rv, _, err := s.Engine.Manifest.CopyRevision(policyID, req.FromRevision)
	if errors.Is(err, repo.ErrPolicyNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", err.Error(), nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	if req.Rules != nil {
		rules, problems := rulesFromReq(req.Rules)
		if len(problems) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":   "invalid_rules",
				"message": "policy rules are not legal lexical patterns",
				"details": problems,
			})
			return
		}
		if err := s.Engine.Manifest.ReplaceRules(rv.ID, rules); err != nil {
			writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
			return
		}
	}
	rules, _ := s.Engine.Manifest.RulesOfRevision(rv.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"policy_id": policyID,
		"revision":  toRevisionResp(rv, rules),
	})
}

type replaceRulesReq struct {
	Rules []ruleReq `json:"rules"`
}

func (s *Server) replaceRules(w http.ResponseWriter, r *http.Request) {
	policyID, revision, ok := parsePolicyIDs(w, r)
	if !ok {
		return
	}
	var req replaceRulesReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	rules, problems := rulesFromReq(req.Rules)
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":   "invalid_rules",
			"message": "policy rules are not legal lexical patterns",
			"details": problems,
		})
		return
	}
	rv, err := s.Engine.Manifest.GetRevision(policyID, revision)
	if errors.Is(err, repo.ErrPolicyNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "revision does not exist", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	if err := s.Engine.Manifest.ReplaceRules(rv.ID, rules); err != nil {
		if errors.Is(err, repo.ErrNotDraft) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "revision_immutable",
				"message": err.Error(),
				"hint":    "POST /v1/policies/" + strconv.FormatInt(policyID, 10) + "/revisions to copy a new draft",
			})
			return
		}
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	rules, _ = s.Engine.Manifest.RulesOfRevision(rv.ID)
	rv.Status = repo.RevDraft
	writeJSON(w, http.StatusOK, map[string]any{
		"policy_id": policyID,
		"revision":  toRevisionResp(rv, rules),
	})
}

func (s *Server) publishRevision(w http.ResponseWriter, r *http.Request) {
	policyID, revision, ok := parsePolicyIDs(w, r)
	if !ok {
		return
	}
	rv, err := s.Engine.Manifest.GetRevision(policyID, revision)
	if errors.Is(err, repo.ErrPolicyNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "revision does not exist", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	// Publishing is the legality gate: a revision with illegal rules can
	// never become usable by snapshots.
	rules, err := s.Engine.Manifest.RulesOfRevision(rv.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	backupRules := make([]backup.PolicyRule, 0, len(rules))
	for _, rr := range rules {
		backupRules = append(backupRules, backup.PolicyRule{Seq: rr.Seq, Action: rr.Action, Pattern: rr.Pattern})
	}
	if err := backup.ValidateRules(backupRules); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":    "invalid_rules",
			"message":  "revision contains illegal rules and cannot be published",
			"details":  []string{err.Error()},
			"revision": revision,
			"status":   repo.RevDraft,
		})
		return
	}
	if err := s.Engine.Manifest.PublishRevision(policyID, revision); err != nil {
		switch {
		case errors.Is(err, repo.ErrPublishConflict):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "publish_conflict",
				"message": err.Error(),
				"hint":    "copy a fresh draft from the published head and re-apply the change",
			})
		case errors.Is(err, repo.ErrNotDraft):
			writeErr(w, http.StatusConflict, "not_a_draft", err.Error(), nil)
		case errors.Is(err, repo.ErrPolicyNotFound):
			writeErr(w, http.StatusNotFound, "not_found", "revision does not exist", nil)
		default:
			writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		}
		return
	}
	rv, _ = s.Engine.Manifest.GetRevision(policyID, revision)
	writeJSON(w, http.StatusOK, map[string]any{
		"policy_id": policyID,
		"revision":  toRevisionResp(rv, rules),
	})
}

func (s *Server) disableRevision(w http.ResponseWriter, r *http.Request) {
	policyID, revision, ok := parsePolicyIDs(w, r)
	if !ok {
		return
	}
	if err := s.Engine.Manifest.DisableRevision(policyID, revision); err != nil {
		if errors.Is(err, repo.ErrPolicyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "revision does not exist", nil)
			return
		}
		writeErr(w, http.StatusConflict, "cannot_disable", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"policy_id": policyID,
		"revision":  revision,
		"status":    repo.RevDisabled,
	})
}

type previewReq struct {
	Root     string    `json:"root"`
	PolicyID *int64    `json:"policy_id"` // preview a stored revision (any status)
	Revision int       `json:"revision"`  // 0 = latest revision of the policy
	Rules    []ruleReq `json:"rules"`     // or inline rules
}

func (s *Server) previewPolicy(w http.ResponseWriter, r *http.Request) {
	var req previewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Root) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "root is required", nil)
		return
	}
	var rules []backup.PolicyRule
	switch {
	case req.Rules != nil:
		repoRules, problems := rulesFromReq(req.Rules)
		if len(problems) > 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error":   "invalid_rules",
				"message": "policy rules are not legal lexical patterns",
				"details": problems,
			})
			return
		}
		for _, rr := range repoRules {
			rules = append(rules, backup.PolicyRule{Seq: rr.Seq, Action: rr.Action, Pattern: rr.Pattern})
		}
	case req.PolicyID != nil:
		// Preview works on any stored revision, drafts included: it changes
		// nothing, so the published-only rule for snapshots does not apply.
		var rv repo.PolicyRevision
		var err error
		if req.Revision == 0 {
			revs, rerr := s.Engine.Manifest.RevisionsOf(*req.PolicyID)
			if rerr == nil && len(revs) == 0 {
				rerr = repo.ErrPolicyNotFound
			}
			if rerr != nil {
				writeErr(w, http.StatusNotFound, "not_found", "policy has no revisions", nil)
				return
			}
			rv = revs[0] // newest first
		} else {
			rv, err = s.Engine.Manifest.GetRevision(*req.PolicyID, req.Revision)
			if errors.Is(err, repo.ErrPolicyNotFound) {
				writeErr(w, http.StatusNotFound, "not_found", "revision does not exist", nil)
				return
			}
			if err != nil {
				writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
				return
			}
		}
		stored, err := s.Engine.Manifest.RulesOfRevision(rv.ID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
			return
		}
		for _, rr := range stored {
			rules = append(rules, backup.PolicyRule{Seq: rr.Seq, Action: rr.Action, Pattern: rr.Pattern})
		}
	default:
		writeErr(w, http.StatusBadRequest, "bad_request", "either rules or policy_id is required", nil)
		return
	}
	res, err := s.Engine.PreviewPolicy(req.Root, rules)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "preview_failed", err.Error(), nil)
		return
	}
	type evResp struct {
		RelPath     string `json:"rel_path"`
		Decision    string `json:"decision"`
		RuleSeq     int    `json:"rule_seq"`
		RuleAction  string `json:"rule_action"`
		RulePattern string `json:"rule_pattern"`
		IsDir       bool   `json:"is_dir"`
	}
	ev := make([]evResp, 0, len(res.Evidence))
	for _, e := range res.Evidence {
		ev = append(ev, evResp{e.RelPath, e.Decision, e.RuleSeq, e.RuleAction, e.RulePattern, e.IsDir})
	}
	errs := make([]map[string]string, 0, len(res.Errors))
	for _, se := range res.Errors {
		errs = append(errs, map[string]string{"rel_path": se.RelPath, "message": se.Message})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"root":      res.Root,
		"files":     res.Files,
		"dirs":      res.Dirs,
		"symlinks":  res.Symlinks,
		"bytes":     res.Bytes,
		"included":  res.Included,
		"truncated": res.Truncated,
		"evidence":  ev,
		"errors":    errs,
	})
}

// selectionEvidence reports the frozen policy and the per-path decisions of
// one snapshot: why each excluded path is absent, and which exceptions kept
// paths that an exclude rule had matched.
func (s *Server) selectionEvidence(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	si, err := s.Engine.Manifest.GetSnapshot(id)
	if errors.Is(err, repo.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "snapshot does not exist", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	frozen, err := s.Engine.Manifest.SnapshotPolicy(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	var policy any
	if frozen != nil {
		policy = map[string]any{
			"policy_id": frozen.PolicyID,
			"revision":  frozen.Revision,
			"rules":     toRuleResp(frozen.Rules),
		}
	}
	records, err := s.Engine.Manifest.SelectionOf(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	type selResp struct {
		RelPath     string `json:"rel_path"`
		Decision    string `json:"decision"`
		RuleSeq     int    `json:"rule_seq"`
		RuleAction  string `json:"rule_action"`
		RulePattern string `json:"rule_pattern"`
		IsDir       bool   `json:"is_dir"`
	}
	sel := make([]selResp, 0, len(records))
	for _, rec := range records {
		sel = append(sel, selResp{rec.RelPath, rec.Decision, rec.RuleSeq, rec.RuleAction, rec.RulePattern, rec.IsDir})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot_id": id,
		"status":      si.Status,
		"policy":      policy,
		"selection":   sel,
	})
}
