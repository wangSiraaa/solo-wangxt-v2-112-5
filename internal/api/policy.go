package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"incbackup/internal/policy"
	"incbackup/internal/repo"
)

type ruleDTO struct {
	Action  string `json:"action"`
	Pattern string `json:"pattern"`
}

type policyReq struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Comment     string    `json:"comment"`
	Rules       []ruleDTO `json:"rules"`
}

type revisionResp struct {
	ID          int64      `json:"id"`
	PolicyID    int64      `json:"policy_id"`
	Number      int64      `json:"number"`
	Status      string     `json:"status"`
	Comment     string     `json:"comment"`
	Rules       []ruleDTO  `json:"rules"`
	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

type policyResp struct {
	ID          int64          `json:"id"`
	Name        string         `json:"name"`
	Status      string         `json:"status"`
	Description string         `json:"description"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Revisions   []revisionResp `json:"revisions,omitempty"`
}

func toRules(dtos []ruleDTO) []policy.Rule {
	out := make([]policy.Rule, len(dtos))
	for i, r := range dtos {
		out[i] = policy.Rule{Action: policy.Action(r.Action), Pattern: r.Pattern}
	}
	return out
}

func toRuleDTOs(b []byte) []ruleDTO {
	var rs []policy.Rule
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil
	}
	out := make([]ruleDTO, len(rs))
	for i, r := range rs {
		out[i] = ruleDTO{Action: string(r.Action), Pattern: r.Pattern}
	}
	return out
}

func toRevisionResp(r repo.Revision) revisionResp {
	return revisionResp{
		ID: r.ID, PolicyID: r.PolicyID, Number: r.Number, Status: r.Status,
		Comment: r.Comment, Rules: toRuleDTOs(r.RulesJSON),
		CreatedAt: r.CreatedAt, PublishedAt: r.PublishedAt,
	}
}

func (s *Server) registerPolicyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/policies", s.createPolicy)
	mux.HandleFunc("GET /v1/policies", s.listPolicies)
	mux.HandleFunc("GET /v1/policies/{id}", s.getPolicy)
	mux.HandleFunc("POST /v1/policies/{id}/retire", s.retirePolicy)
	mux.HandleFunc("POST /v1/policies/{id}/revisions", s.copyRevision)
	mux.HandleFunc("GET /v1/revisions/{rid}", s.getRevision)
	mux.HandleFunc("PUT /v1/revisions/{rid}", s.updateRevision)
	mux.HandleFunc("POST /v1/revisions/{rid}/publish", s.publishRevision)
	mux.HandleFunc("POST /v1/policies/preview", s.preview)
	mux.HandleFunc("GET /v1/snapshots/{id}/selection", s.selection)
}

func decodePolicyReq(w http.ResponseWriter, r *http.Request) (policyReq, bool) {
	var req policyReq
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
			return req, false
		}
	}
	return req, true
}

func writeValidationError(w http.ResponseWriter, err error) {
	var ve *policy.ValidationError
	if errors.As(err, &ve) {
		writeErr(w, http.StatusBadRequest, "invalid_rules", err.Error(), ve.Errors)
		return
	}
	writeErr(w, http.StatusBadRequest, "bad_request", err.Error(), nil)
}

func (s *Server) createPolicy(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePolicyReq(w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "name is required", nil)
		return
	}
	pid, rid, err := s.Engine.CreatePolicy(req.Name, req.Description, req.Comment, toRules(req.Rules))
	if err != nil {
		writeValidationError(w, err)
		return
	}
	rev, err := s.Engine.Manifest.GetRevision(rid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"policy_id": pid,
		"revision":  toRevisionResp(rev),
	})
}

func (s *Server) listPolicies(w http.ResponseWriter, r *http.Request) {
	all, err := s.Engine.Manifest.ListPolicies()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	out := make([]policyResp, 0, len(all))
	for _, p := range all {
		out = append(out, policyResp{
			ID: p.ID, Name: p.Name, Status: p.Status, Description: p.Description,
			CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": out})
}

func parsePolicyID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_id", "policy id must be an integer", nil)
		return 0, false
	}
	return id, true
}

func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePolicyID(w, r)
	if !ok {
		return
	}
	p, err := s.Engine.Manifest.GetPolicy(id)
	if errors.Is(err, repo.ErrPolicyNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "policy does not exist", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	revs, err := s.Engine.Manifest.ListRevisions(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	rr := make([]revisionResp, 0, len(revs))
	for _, rv := range revs {
		rr = append(rr, toRevisionResp(rv))
	}
	writeJSON(w, http.StatusOK, policyResp{
		ID: p.ID, Name: p.Name, Status: p.Status, Description: p.Description,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Revisions: rr,
	})
}

func (s *Server) retirePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := parsePolicyID(w, r)
	if !ok {
		return
	}
	if err := s.Engine.RetirePolicy(id); err != nil {
		if errors.Is(err, repo.ErrPolicyNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "policy does not exist", nil)
			return
		}
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policy_id": id, "status": repo.PolicyRetired})
}

type copyReq struct {
	SourceRevision int64  `json:"source_revision_id"`
	Comment        string `json:"comment"`
}

// copyRevision handles POST /v1/policies/{id}/revisions. With a
// source_revision_id it copies that revision's rules into a new draft; with
// "rules" in the body it starts a draft from an explicitly given rule set.
func (s *Server) copyRevision(w http.ResponseWriter, r *http.Request) {
	pid, ok := parsePolicyID(w, r)
	if !ok {
		return
	}
	var body struct {
		copyReq
		Rules []ruleDTO `json:"rules"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
			return
		}
	}
	var rid, num int64
	var err error
	if body.SourceRevision != 0 {
		// ensure source belongs to this policy
		src, gerr := s.Engine.Manifest.GetRevision(body.SourceRevision)
		if gerr != nil {
			writeErr(w, http.StatusNotFound, "not_found", gerr.Error(), nil)
			return
		}
		if src.PolicyID != pid {
			writeErr(w, http.StatusBadRequest, "bad_request",
				"source revision belongs to a different policy", nil)
			return
		}
		rid, num, _, err = s.Engine.CopyRevision(body.SourceRevision, body.Comment)
	} else {
		rid, num, err = s.Engine.Manifest.AddDraftRevision(pid, body.Comment, mustRulesJSON(toRules(body.Rules)))
	}
	if err != nil {
		if errors.Is(err, repo.ErrPolicyRetired) {
			writeErr(w, http.StatusConflict, "policy_retired", err.Error(), nil)
			return
		}
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	rev, _ := s.Engine.Manifest.GetRevision(rid)
	writeJSON(w, http.StatusCreated, map[string]any{
		"policy_id": pid, "number": num, "revision": toRevisionResp(rev),
	})
}

func mustRulesJSON(rules []policy.Rule) []byte {
	b, _ := json.Marshal(rules)
	return b
}

func parseRevisionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_id", "revision id must be an integer", nil)
		return 0, false
	}
	return id, true
}

func (s *Server) getRevision(w http.ResponseWriter, r *http.Request) {
	rid, ok := parseRevisionID(w, r)
	if !ok {
		return
	}
	rev, err := s.Engine.Manifest.GetRevision(rid)
	if errors.Is(err, repo.ErrRevisionNotFound) {
		writeErr(w, http.StatusNotFound, "not_found", "revision does not exist", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, toRevisionResp(rev))
}

func (s *Server) updateRevision(w http.ResponseWriter, r *http.Request) {
	rid, ok := parseRevisionID(w, r)
	if !ok {
		return
	}
	req, ok := decodePolicyReq(w, r)
	if !ok {
		return
	}
	if err := s.Engine.UpdateDraft(rid, req.Comment, toRules(req.Rules)); err != nil {
		if errors.Is(err, repo.ErrRevisionNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "revision does not exist", nil)
			return
		}
		if errors.Is(err, repo.ErrNotDraft) {
			writeErr(w, http.StatusConflict, "revision_immutable",
				"published revisions cannot be edited; copy into a new revision", map[string]any{
					"hint":            "POST /v1/policies/{id}/revisions with source_revision_id",
					"source_revision": rid,
				})
			return
		}
		writeValidationError(w, err)
		return
	}
	rev, _ := s.Engine.Manifest.GetRevision(rid)
	writeJSON(w, http.StatusOK, toRevisionResp(rev))
}

func (s *Server) publishRevision(w http.ResponseWriter, r *http.Request) {
	rid, ok := parseRevisionID(w, r)
	if !ok {
		return
	}
	rev, err := s.Engine.PublishPolicyRevision(rid)
	if err != nil {
		if errors.Is(err, repo.ErrRevisionNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "revision does not exist", nil)
			return
		}
		if errors.Is(err, repo.ErrNotDraft) || errors.Is(err, repo.ErrNotPublished) {
			writeErr(w, http.StatusConflict, "publish_conflict", err.Error(), map[string]any{
				"hint": "only one revision per policy may be published; the losing draft was not changed",
			})
			return
		}
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	writeJSON(w, http.StatusOK, toRevisionResp(*rev))
}

type previewReq struct {
	Root  string    `json:"root"`
	Rules []ruleDTO `json:"rules"`
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	var req previewReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_json", err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Root) == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "root is required", nil)
		return
	}
	items, included, excluded, err := s.Engine.Preview(req.Root, toRules(req.Rules))
	if err != nil {
		writeValidationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"root": req.Root, "included": included, "excluded": excluded, "paths": items,
	})
}

type hitResp struct {
	Order    int    `json:"order"`
	Action   string `json:"action"`
	Pattern  string `json:"pattern"`
	Decisive bool   `json:"decisive"`
}

func (s *Server) selection(w http.ResponseWriter, r *http.Request) {
	id, ok := parseID(w, r)
	if !ok {
		return
	}
	if _, err := s.Engine.Manifest.GetSnapshot(id); err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "not_found", "snapshot does not exist", nil)
			return
		}
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	freeze, err := s.Engine.Manifest.GetFreeze(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	recs, err := s.Engine.Manifest.ListSelection(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "db", err.Error(), nil)
		return
	}
	var freezeOut any
	if freeze != nil {
		freezeOut = map[string]any{
			"policy_id":       freeze.PolicyID,
			"policy_name":     freeze.PolicyName,
			"revision_id":     freeze.RevisionID,
			"revision_number": freeze.RevisionNumber,
			"status":          freeze.StatusAtFreeze,
			"frozen_at":       freeze.FrozenAt,
			"rules":           toRuleDTOs(freeze.RulesJSON),
		}
	}
	type selItem struct {
		RelPath         string    `json:"rel_path"`
		KindHint        string    `json:"kind_hint"`
		Included        bool      `json:"included"`
		FilterDefault   bool      `json:"filter_default,omitempty"`
		ExcludedBy      string    `json:"excluded_by,omitempty"`
		DecisiveOrder   int       `json:"decisive_order,omitempty"`
		DecisiveAction  string    `json:"decisive_action,omitempty"`
		DecisivePattern string    `json:"decisive_pattern,omitempty"`
		Hits            []hitResp `json:"hits,omitempty"`
	}
	out := make([]selItem, 0, len(recs))
	for _, rec := range recs {
		it := selItem{
			RelPath: rec.RelPath, KindHint: rec.KindHint, Included: rec.Included,
			FilterDefault: rec.FilterDefault, ExcludedBy: rec.ExcludedBy,
			DecisiveOrder:  rec.DecisiveOrder,
			DecisiveAction: rec.DecisiveAction, DecisivePattern: rec.DecisivePattern,
		}
		for _, h := range rec.Hits {
			it.Hits = append(it.Hits, hitResp{
				Order: h.Order, Action: h.Action, Pattern: h.Pattern, Decisive: h.Decisive,
			})
		}
		out = append(out, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot_id":     id,
		"frozen_revision": freezeOut,
		"selection":       out,
	})
}
