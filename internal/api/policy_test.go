package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"incbackup/internal/api"
	"incbackup/internal/backup"
	"incbackup/internal/repo"
)

func newServer(t *testing.T) (*httptest.Server, string, *backup.Engine) {
	t.Helper()
	dir := t.TempDir()
	m, err := repo.OpenManifest(filepath.Join(dir, "m.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	s, err := repo.NewContentStore(filepath.Join(dir, "chunks"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := backup.NewEngine(m, s)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Engine: e}).NewRouter())
	t.Cleanup(srv.Close)
	return srv, dir, e
}

func do(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	dec := json.NewDecoder(resp.Body)
	_ = dec.Decode(&out)
	return resp.StatusCode, out
}

func TestPolicyAPILifecycleAndSnapshotEvidence(t *testing.T) {
	srv, dir, _ := newServer(t)
	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(src, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{
		"a.tmp": "A", "keep.tmp": "K", "f.txt": "f", "cache/b.tmp": "B",
	} {
		if err := os.WriteFile(filepath.Join(src, filepath.FromSlash(p)), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// illegal rules rejected, nothing created
	code, body := do(t, "POST", srv.URL+"/v1/policies", map[string]any{
		"name": "bad",
		"rules": []map[string]string{
			{"action": "exclude", "pattern": "../x"},
		},
	})
	if code != http.StatusBadRequest || body["error"] != "invalid_rules" {
		t.Fatalf("illegal rules: code=%d body=%v", code, body)
	}

	// preview before publishing
	code, body = do(t, "POST", srv.URL+"/v1/policies/preview", map[string]any{
		"root": src,
		"rules": []map[string]string{
			{"action": "exclude", "pattern": "*.tmp"},
			{"action": "exception", "pattern": "keep.tmp"},
		},
	})
	if code != 200 {
		t.Fatalf("preview: %d %v", code, body)
	}
	// excluded: a.tmp, cache/b.tmp (2); included: cache(dir), f.txt, keep.tmp (3)
	if body["excluded"].(float64) != 2 || body["included"].(float64) != 3 {
		t.Fatalf("preview counts wrong: included=%v excluded=%v", body["included"], body["excluded"])
	}

	// create policy draft
	code, body = do(t, "POST", srv.URL+"/v1/policies", map[string]any{
		"name":        "no-tmp",
		"description": "drop temp files",
		"rules": []map[string]string{
			{"action": "exclude", "pattern": "*.tmp"},
			{"action": "exception", "pattern": "keep.tmp"},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	policyID := int64(body["policy_id"].(float64))
	rev := body["revision"].(map[string]any)
	draftID := int64(rev["id"].(float64))
	if rev["status"] != "draft" {
		t.Fatalf("new revision status=%v", rev["status"])
	}

	// snapshot with draft => 422, no committed snapshot
	code, body = do(t, "POST", srv.URL+"/v1/snapshots", map[string]any{
		"root": src, "policy_revision_id": draftID,
	})
	if code != http.StatusUnprocessableEntity || body["error"] != "invalid_policy" {
		t.Fatalf("draft snapshot: %d %v", code, body)
	}

	// publishing an illegal update fails, draft stays
	code, body = do(t, "PUT", srv.URL+"/v1/revisions/"+itoa(draftID), map[string]any{
		"rules": []map[string]string{{"action": "exclude", "pattern": "/absolute"}},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("illegal update: %d %v", code, body)
	}

	// publish
	code, body = do(t, "POST", srv.URL+"/v1/revisions/"+itoa(draftID)+"/publish", nil)
	if code != http.StatusOK || body["status"] != "published" {
		t.Fatalf("publish: %d %v", code, body)
	}

	// edit after publish => 409 immutable
	code, body = do(t, "PUT", srv.URL+"/v1/revisions/"+itoa(draftID), map[string]any{
		"rules": []map[string]string{{"action": "exclude", "pattern": "*.log"}},
	})
	if code != http.StatusConflict || body["error"] != "revision_immutable" {
		t.Fatalf("immutable: %d %v", code, body)
	}

	// snapshot under published revision
	code, body = do(t, "POST", srv.URL+"/v1/snapshots", map[string]any{
		"root": src, "message": "scoped", "policy_revision_id": draftID,
	})
	if code != http.StatusCreated || body["status"] != "committed" {
		t.Fatalf("snapshot: %d %v", code, body)
	}
	snapID := int64(body["snapshot_id"].(float64))
	if body["policy_revision_id"].(float64) != float64(draftID) {
		t.Fatal("response must name the frozen revision")
	}

	// selection evidence
	code, body = do(t, "GET", srv.URL+"/v1/snapshots/"+itoa(snapID)+"/selection", nil)
	if code != 200 {
		t.Fatalf("selection: %d %v", code, body)
	}
	fz := body["frozen_revision"].(map[string]any)
	if fz["revision_id"].(float64) != float64(draftID) || len(fz["rules"].([]any)) != 2 {
		t.Fatalf("frozen revision wrong: %v", fz)
	}
	var excluded, exceptionKept []string
	for _, x := range body["selection"].([]any) {
		m := x.(map[string]any)
		if m["included"].(bool) {
			exceptionKept = append(exceptionKept, m["rel_path"].(string))
		} else {
			excluded = append(excluded, m["rel_path"].(string))
		}
	}
	if len(excluded) != 2 { // a.tmp and cache/b.tmp
		t.Fatalf("excluded evidence = %v", excluded)
	}
	if len(exceptionKept) != 1 || exceptionKept[0] != "keep.tmp" {
		t.Fatalf("exception evidence = %v", exceptionKept)
	}

	// restore tree obeys rules
	target := filepath.Join(dir, "restored")
	code, body = do(t, "POST", srv.URL+"/v1/snapshots/"+itoa(snapID)+"/restore",
		map[string]any{"target": target})
	if code != http.StatusCreated {
		t.Fatalf("restore: %d %v", code, body)
	}
	if _, err := os.ReadFile(filepath.Join(target, "keep.tmp")); err != nil {
		t.Errorf("keep.tmp restored: %v", err)
	}
	for _, gone := range []string{"a.tmp", "cache/b.tmp"} {
		if _, err := os.Lstat(filepath.Join(target, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s must not be restored", gone)
		}
	}

	// copy -> modify -> publish revision 2
	code, body = do(t, "POST", srv.URL+"/v1/policies/"+itoa(policyID)+"/revisions",
		map[string]any{"source_revision_id": draftID, "comment": "copy"})
	if code != http.StatusCreated {
		t.Fatalf("copy: %d %v", code, body)
	}
	rev2 := body["revision"].(map[string]any)
	rid2 := int64(rev2["id"].(float64))
	if body["number"].(float64) != 2 {
		t.Fatalf("revision number=%v want 2", body["number"])
	}
	code, _ = do(t, "PUT", srv.URL+"/v1/revisions/"+itoa(rid2), map[string]any{
		"rules": []map[string]string{
			{"action": "exclude", "pattern": "*.tmp"},
			{"action": "exception", "pattern": "keep.tmp"},
			{"action": "exclude", "pattern": "cache/"},
		},
	})
	if code != 200 {
		t.Fatalf("update draft 2: %d", code)
	}
	code, body = do(t, "POST", srv.URL+"/v1/revisions/"+itoa(rid2)+"/publish", nil)
	if code != 200 {
		t.Fatalf("publish v2: %d %v", code, body)
	}

	// old snapshot still shows the old frozen rules
	code, body = do(t, "GET", srv.URL+"/v1/snapshots/"+itoa(snapID)+"/selection", nil)
	fz = body["frozen_revision"].(map[string]any)
	if fz["revision_number"].(float64) != 1 || len(fz["rules"].([]any)) != 2 {
		t.Fatalf("old freeze mutated: %v", fz)
	}

	// retire => new snapshots rejected
	code, _ = do(t, "POST", srv.URL+"/v1/policies/"+itoa(policyID)+"/retire", nil)
	if code != 200 {
		t.Fatalf("retire: %d", code)
	}
	code, body = do(t, "POST", srv.URL+"/v1/snapshots", map[string]any{
		"root": src, "policy_revision_id": rid2,
	})
	if code != http.StatusUnprocessableEntity {
		t.Fatalf("retired snapshot: %d %v", code, body)
	}
	// old snapshot still restorable after retirement
	code, _ = do(t, "POST", srv.URL+"/v1/snapshots/"+itoa(snapID)+"/restore",
		map[string]any{"target": filepath.Join(dir, "restored-2")})
	if code != http.StatusCreated {
		t.Fatalf("old snapshot restore after retire: %d", code)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
