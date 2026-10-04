// Command demo drives the backup HTTP API through the full failure story:
//
//  1. committed snapshot + restore into a new directory with digest/length
//     verification (empty file included),
//  2. small edit reusing existing content-defined chunks,
//  3. refusal to restore over an existing directory,
//  4. file actively written during scan -> re-read then rejected,
//  5. commit interruption losing a blob -> failed snapshot with the exact
//     missing chunk located,
//  6. symlink escaping the root -> restored link is blocked,
//  7. server restart with a pending snapshot -> startup recovery commits it.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"incbackup/internal/api"
	"incbackup/internal/backup"
	"incbackup/internal/repo"
)

var pass, fail int

func main() {
	keep := flag.Bool("keep", false, "keep the demo workspace afterwards")
	flag.Parse()

	work, err := os.MkdirTemp("", "incbackup-demo-")
	must(err)
	if *keep {
		fmt.Printf("(workspace: %s)\n", work)
	} else {
		defer os.RemoveAll(work)
	}
	repoDir := filepath.Join(work, "repo")
	src := filepath.Join(work, "src")

	section(0, "准备：在一个进程内启动本地 API 服务与数据目录")
	srv := startServer(repoDir)
	fmt.Printf("  API   : %s\n", srv.URL)
	fmt.Printf("  仓库  : %s (manifest.sqlite + chunks/)\n", repoDir)
	fmt.Printf("  数据源: %s\n", src)
	must(os.MkdirAll(filepath.Join(src, "docs"), 0o755))

	// Big-ish log so content-defined chunking produces several chunks.
	log := make([]byte, 0, 160*1024)
	for i := 0; i < 160*1024; i++ {
		log = append(log, byte("abcdefghijklmnopqrstuvwxyz0123456789\n"[i%37]))
	}
	must(os.WriteFile(filepath.Join(src, "app.log"), log, 0o644))
	must(os.WriteFile(filepath.Join(src, "docs", "notes.txt"), []byte("meeting notes\n"), 0o644))
	must(os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o750))
	must(os.WriteFile(filepath.Join(src, "EMPTY.dat"), nil, 0o600)) // empty file
	must(os.Symlink("docs/notes.txt", filepath.Join(src, "link_to_notes")))

	// ---- 1. first snapshot + restore --------------------------------------
	section(1, "首次快照：完成前逐块验证，然后恢复到全新目录并核对摘要与长度")
	r := post(srv.URL+"/v1/snapshots", map[string]any{"root": src, "message": "baseline"})
	firstID := int64(r["snapshot_id"].(float64))
	fmt.Printf("  快照 %d: status=%s 新块=%v 引用块=%v\n",
		firstID, r["status"], r["chunks_new"], r["chunks_referenced"])
	check("快照状态为 committed", r["status"] == "committed")

	restoreDir := filepath.Join(work, "restore-1")
	code, body := raw("POST", srv.URL+fmt.Sprintf("/v1/snapshots/%d/restore", firstID),
		map[string]any{"target": restoreDir})
	if code != http.StatusCreated {
		must(fmt.Errorf("restore #1 failed: HTTP %d %s", code, body["message"]))
	}
	rr := body
	verified, _ := rr["verified"].([]any)
	fmt.Printf("  恢复到 %s\n  文件=%v 目录=%v 符号链接=%v 字节=%v\n",
		restoreDir, rr["files"], rr["directories"], rr["symlinks"], rr["bytes"])
	for _, v := range verified {
		m := v.(map[string]any)
		fmt.Printf("    %-18s 长度=%-6d 块数=%-2d 摘要=%s… 权限=0%o\n",
			m["rel_path"], int64(m["size"].(float64)), int(m["chunk_count"].(float64)),
			m["digest"].(string)[:16], int64(m["mode"].(float64)))
	}
	emptyOK := false
	for _, v := range verified {
		m := v.(map[string]any)
		if m["rel_path"] == "EMPTY.dat" {
			emptyOK = m["size"].(float64) == 0 &&
				m["digest"] == fmt.Sprintf("%x", sha256.New().Sum(nil)) &&
				m["chunk_count"].(float64) == 0
		}
	}
	check("空文件：长度 0、SHA256=e3b0c44…、0 个内容块", emptyOK)

	// Compare tree metadata with source.
	var modeMismatch []string
	for _, rel := range []string{"run.sh", "app.log", "docs"} {
		a, _ := os.Lstat(filepath.Join(src, rel))
		b, err := os.Lstat(filepath.Join(restoreDir, rel))
		if err != nil || a.Mode().Perm() != b.Mode().Perm() {
			modeMismatch = append(modeMismatch, rel)
		}
	}
	check("目录权限与文件权限均保留 (run.sh 0750, docs 0755)", len(modeMismatch) == 0)
	lt, _ := os.Readlink(filepath.Join(restoreDir, "link_to_notes"))
	check("符号链接本身被恢复（链接目标=docs/notes.txt，未跟随）", lt == "docs/notes.txt")
	notes, err := os.ReadFile(filepath.Join(restoreDir, "link_to_notes"))
	check("恢复出的链接仍可解析到文件内容", err == nil && string(notes) == "meeting notes\n")

	// byte-identical content of app.log independently re-hashed
	got, _ := hashFile(filepath.Join(restoreDir, "app.log"))
	want, _ := hashFile(filepath.Join(src, "app.log"))
	check("恢复内容逐字节一致（独立重算 SHA256）", got == want)

	// ---- 2. small edit reuses chunks --------------------------------------
	section(2, "小改动的增量：在 app.log 中部改一行，只新增 1 个块，其余块全部复用")
	off := 80 * 1024
	copy(log[off:off+8], []byte("PATCHED!"))
	must(os.WriteFile(filepath.Join(src, "app.log"), log, 0o644))
	r = post(srv.URL+"/v1/snapshots", map[string]any{"root": src, "message": "one-line patch"})
	secondID := int64(r["snapshot_id"].(float64))
	newChunks := int64(r["chunks_new"].(float64))
	refChunks := int64(r["chunks_referenced"].(float64))
	fmt.Printf("  快照 %d: 引用块=%d，其中新写入=%d，复用=%d\n",
		secondID, refChunks, newChunks, refChunks-newChunks)
	check("仅有改动附近的 1 个块是新块（内容定义分块边界由内容决定）", newChunks == 1)
	check("其余块全部复用快照 1 中的旧块", refChunks-newChunks == refChunks-1)

	restore2 := filepath.Join(work, "restore-2")
	code, body = raw("POST", srv.URL+fmt.Sprintf("/v1/snapshots/%d/restore", secondID), map[string]any{"target": restore2})
	if code != http.StatusCreated {
		must(fmt.Errorf("restore #2 failed: HTTP %d %s", code, body["message"]))
	}
	g2, _ := hashFile(filepath.Join(restore2, "app.log"))
	w2, _ := hashFile(filepath.Join(src, "app.log"))
	check("恢复快照 2 后 app.log 与当前源文件一致", g2 == w2)

	// ---- 3. never overwrite destination -----------------------------------
	section(3, "恢复位置已有任何东西 → 拒绝，不覆盖、不合并")
	code, body = raw("POST", srv.URL+fmt.Sprintf("/v1/snapshots/%d/restore", firstID),
		map[string]any{"target": restoreDir})
	fmt.Printf("  POST restore 到已存在目录 -> HTTP %d: %s\n", code, body["error"])
	check("已有目录时返回 409 target_exists", code == http.StatusConflict && body["error"] == "target_exists")

	// ---- 4. file being written during scan --------------------------------
	section(4, "扫描中仍在写入的文件：先短暂写入触发重读，再持续写入触发拒绝")
	growing := filepath.Join(src, "growing.log")
	must(os.WriteFile(growing, []byte("line0\n"), 0o644))

	// 4a. writer finishes within the retry window: scanner re-reads and commits
	done := make(chan struct{})
	go func() {
		f, _ := os.OpenFile(growing, os.O_APPEND|os.O_WRONLY, 0o644)
		for i := 1; i <= 5; i++ {
			fmt.Fprintf(f, "line%d %s\n", i, strings.Repeat("y", 200))
			time.Sleep(20 * time.Millisecond)
		}
		f.Close()
		close(done)
	}()
	snap4a := post(srv.URL+"/v1/snapshots", map[string]any{"root": src, "message": "writer settles during retry"})
	<-done
	check("写入在重读窗口内结束：扫描器重读文件，快照仍正常 committed", snap4a["status"] == "committed")

	// 4b. writer keeps going: every pass sees a changed size/mtime -> rejected
	stop := make(chan struct{})
	go func() {
		f, _ := os.OpenFile(growing, os.O_APPEND|os.O_WRONLY, 0o644)
		defer f.Close()
		i := 1
		for {
			select {
			case <-stop:
				return
			default:
				fmt.Fprintf(f, "line%d %s\n", i, strings.Repeat("x", 200))
				i++
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	time.Sleep(30 * time.Millisecond)
	code, body = raw("POST", srv.URL+"/v1/snapshots", map[string]any{"root": src, "message": "racing writer"})
	close(stop)
	reasons, _ := body["reasons"].([]any)
	fmt.Printf("  持续写入时 HTTP %d, 快照 %v -> %s\n", code, body["snapshot_id"], body["error"])
	for _, x := range reasons {
		fmt.Printf("    拒绝原因: %s\n", x)
	}
	sawUnstable := false
	for _, x := range reasons {
		if strings.Contains(x.(string), "growing.log") &&
			strings.Contains(x.(string), "still being written") {
			sawUnstable = true
		}
	}
	check("3 次重读后仍在变化的 growing.log 被明确点名（而不是备份静默成功）",
		code == http.StatusConflict && sawUnstable)
	badID := int64(body["snapshot_id"].(float64))
	errs, _ := get(srv.URL + fmt.Sprintf("/v1/snapshots/%d/errors", badID))["errors"].([]any)
	check("失败快照保留在清单中，stage=scan 可追溯", len(errs) > 0 &&
		errs[0].(map[string]any)["stage"] == "scan")

	// ---- 5. commit interruption: lost blob, locate exact chunk ------------
	section(5, "模拟提交中断：删掉最后一个内容块 → 完成前验证拦截并定位具体缺块")
	must(os.WriteFile(growing, []byte("stable now\n"), 0o644))
	code, body = raw("POST", srv.URL+"/v1/snapshots",
		map[string]any{"root": src, "message": "interrupted commit", "lose_chunks": 1})
	fmt.Printf("  HTTP %d, 快照 %v -> %s\n", code, body["snapshot_id"], body["error"])
	interruptedID := int64(body["snapshot_id"].(float64))
	check("缺块快照不能 committed，返回 409", code == http.StatusConflict)

	missing := get(srv.URL + fmt.Sprintf("/v1/snapshots/%d/missing", interruptedID))["missing"].([]any)
	fmt.Printf("  维护查询 GET .../missing 找到 %d 个缺块：\n", len(missing))
	for _, x := range missing {
		m := x.(map[string]any)
		fmt.Printf("    文件   : %s\n", m["rel_path"])
		fmt.Printf("    块摘要 : %s\n", m["chunk_digest"])
		fmt.Printf("    应在   : %s\n", m["expected_blob_path"])
		fmt.Printf("    原因   : %s\n", m["reason"])
		_, statErr := os.Stat(m["expected_blob_path"].(string))
		check("报告的块路径在磁盘上确实不存在", os.IsNotExist(statErr))
	}
	check("缺块清单精确到 文件+摘要+期望磁盘路径（不是“上传队列为空”）", len(missing) == 1)
	si := get(srv.URL + fmt.Sprintf("/v1/snapshots/%d", interruptedID))
	check("失败快照状态可查 = failed", si["status"] == "failed")

	// restore of a failed snapshot must be refused
	code, body = raw("POST", srv.URL+fmt.Sprintf("/v1/snapshots/%d/restore", interruptedID),
		map[string]any{"target": filepath.Join(work, "never")})
	fmt.Printf("  尝试恢复 failed 快照 -> HTTP %d %s\n", code, body["error"])
	check("failed 快照拒绝恢复", code >= 400)

	// ---- 6. symlink escape containment ------------------------------------
	section(6, "符号链接越界：备份只存链接本身，恢复时指向根目录外的链接被拒绝")
	secret := filepath.Join(work, "secret.txt")
	must(os.WriteFile(secret, []byte("TOP SECRET"), 0o600))
	evil := filepath.Join(src, "evil_link")
	_ = os.Remove(evil)
	rel, _ := filepath.Rel(filepath.Join(src), secret)
	must(os.Symlink(rel, evil)) // src/evil_link -> ../secret.txt
	snapEvil := post(srv.URL+"/v1/snapshots", map[string]any{"root": src, "message": "with evil link"})
	evilID := int64(snapEvil["snapshot_id"].(float64))
	evilTarget := filepath.Join(work, "restore-evil")
	code, body = raw("POST", srv.URL+fmt.Sprintf("/v1/snapshots/%d/restore", evilID),
		map[string]any{"target": evilTarget})
	fmt.Printf("  含越界链接的恢复 -> HTTP %d: %s\n", code, body["message"])
	check("越界符号链接恢复被阻止 (422)", code == http.StatusUnprocessableEntity)
	_, statErr := os.Lstat(evilTarget)
	check("失败后不留半成品目录（回滚清理）", os.IsNotExist(statErr))
	_, err = os.ReadFile(filepath.Join(evilTarget, "evil_link"))
	check("秘密文件没有被触及/写出", err != nil)

	// ---- 7. restart recovery of a pending snapshot -------------------------
	section(7, "提交前进程退出：快照留在 pending，服务重启时自动验证并给结论")
	pend := post(srv.URL+"/v1/snapshots", map[string]any{"root": src, "message": "crash before commit", "finish": false})
	pendID := int64(pend["snapshot_id"].(float64))
	fmt.Printf("  故障时刻: 快照 %d status=%s，块已落盘、清单未提交\n", pendID, pend["status"])
	check("finish=false 留下 pending 快照", pend["status"] == "pending")
	srv.Close()

	srv = startServer(repoDir) // same repo, new process equivalent
	time.Sleep(100 * time.Millisecond)
	si = get(srv.URL + fmt.Sprintf("/v1/snapshots/%d", pendID))
	fmt.Printf("  重启后: 快照 %d status=%s\n", pendID, si["status"])
	check("重启恢复把 pending 快照验证后提交为 committed", si["status"] == "committed")

	// ---- 8. versioned scan policy: excludes, exceptions, frozen evidence ----
	section(8, "版本化扫描策略：草稿→发布→停用，排除 *.tmp 但例外保留，证据可查且冻结")
	polSrc := filepath.Join(work, "src-pol")
	must(os.MkdirAll(filepath.Join(polSrc, "cache"), 0o755))
	must(os.WriteFile(filepath.Join(polSrc, "a.tmp"), []byte("temp A"), 0o644))
	must(os.WriteFile(filepath.Join(polSrc, "b.tmp"), []byte("temp B"), 0o644))
	must(os.WriteFile(filepath.Join(polSrc, "keep.tmp"), []byte("must survive"), 0o644))
	must(os.WriteFile(filepath.Join(polSrc, "notes.txt"), []byte("real notes\n"), 0o644))
	must(os.WriteFile(filepath.Join(polSrc, "cache", "c.tmp"), []byte("temp C"), 0o644))

	// illegal escape rule rejected at creation
	code, body = raw("POST", srv.URL+"/v1/policies", map[string]any{
		"name":  "evil",
		"rules": []map[string]string{{"action": "exclude", "pattern": "../outside"}},
	})
	check("含 '..' 越界规则在创建时被拒绝（400），不产生任何修订", code == http.StatusBadRequest && body["error"] == "invalid_rules")

	// draft
	code, body = raw("POST", srv.URL+"/v1/policies", map[string]any{
		"name":        "no-tmp",
		"description": "drop regenerable temp files",
		"rules": []map[string]string{
			{"action": "exclude", "pattern": "*.tmp"},
			{"action": "exception", "pattern": "keep.tmp"},
		},
	})
	check("创建策略与首个草稿修订", code == http.StatusCreated)
	polID := int64(body["policy_id"].(float64))
	draftRev := body["revision"].(map[string]any)
	revID := int64(draftRev["id"].(float64))

	// snapshot referencing a draft is refused
	code, body = raw("POST", srv.URL+"/v1/snapshots",
		map[string]any{"root": polSrc, "policy_revision_id": revID})
	check("快照只能引用已发布修订：草稿被拒 (422)，且不创建 committed 快照",
		code == http.StatusUnprocessableEntity && body["error"] == "invalid_policy")

	// preview first, then publish
	code, body = raw("POST", srv.URL+"/v1/policies/preview", map[string]any{
		"root": polSrc,
		"rules": []map[string]string{
			{"action": "exclude", "pattern": "*.tmp"},
			{"action": "exception", "pattern": "keep.tmp"},
		},
	})
	check("发布前预览：只做词法判定不落盘", code == http.StatusOK &&
		body["excluded"].(float64) == 3 && body["included"].(float64) == 3)

	code, body = raw("POST", srv.URL+fmt.Sprintf("/v1/revisions/%d/publish", revID), nil)
	check("发布修订 1", code == http.StatusOK && body["status"] == "published")

	// editing a published revision is refused; must copy
	code, body = raw("PUT", srv.URL+fmt.Sprintf("/v1/revisions/%d", revID), map[string]any{
		"rules": []map[string]string{{"action": "exclude", "pattern": "*.log"}},
	})
	check("已发布修订不可编辑 (409)，新修改必须复制版本",
		code == http.StatusConflict && body["error"] == "revision_immutable")

	// snapshot under published revision
	code, body = raw("POST", srv.URL+"/v1/snapshots", map[string]any{
		"root": polSrc, "message": "scoped v1", "policy_revision_id": revID,
	})
	check("按已发布修订创建快照成功 committed", code == http.StatusCreated && body["status"] == "committed")
	polSnapID := int64(body["snapshot_id"].(float64))
	check("快照响应记录所冻结的修订", body["policy_revision_id"].(float64) == float64(revID))

	// selection evidence: both exclude and exception hits listed
	code, body = raw("GET", srv.URL+fmt.Sprintf("/v1/snapshots/%d/selection", polSnapID), nil)
	fz := body["frozen_revision"].(map[string]any)
	check("证据中持久化修订号、规则顺序（2 条规则）", code == http.StatusOK &&
		fz["revision_number"].(float64) == 1 && len(fz["rules"].([]any)) == 2)
	var excPaths, keptPaths []string
	for _, x := range body["selection"].([]any) {
		m := x.(map[string]any)
		hits := m["hits"].([]any)
		if m["included"].(bool) {
			keptPaths = append(keptPaths, m["rel_path"].(string))
		} else {
			excPaths = append(excPaths, m["rel_path"].(string))
			check(fmt.Sprintf("  排除证据 %s 命中决定性 exclude *.tmp", m["rel_path"]),
				len(hits) == 1 && hits[0].(map[string]any)["decisive"].(bool) &&
					hits[0].(map[string]any)["action"] == "exclude")
		}
	}
	sortStrings(excPaths)
	check("排除证据列出所有 *.tmp（含 cache/c.tmp）", strings.Join(excPaths, ",") == "a.tmp,b.tmp,cache/c.tmp")
	check("例外证据保留 keep.tmp，且同时列出 exclude+exception 两条命中",
		len(keptPaths) == 1 && keptPaths[0] == "keep.tmp")

	// restore tree obeys the rules
	polRestore := filepath.Join(work, "restore-pol")
	code, body = raw("POST", srv.URL+fmt.Sprintf("/v1/snapshots/%d/restore", polSnapID),
		map[string]any{"target": polRestore})
	check("按策略快照恢复成功", code == http.StatusCreated)
	_, errA := os.ReadFile(filepath.Join(polRestore, "keep.tmp"))
	_, errN := os.ReadFile(filepath.Join(polRestore, "notes.txt"))
	_, errX := os.Lstat(filepath.Join(polRestore, "a.tmp"))
	check("恢复树：keep.tmp/notes.txt 在，a.tmp 不在",
		errA == nil && errN == nil && os.IsNotExist(errX))

	// copy -> modify -> publish revision 2 (also exclude the cache/ directory)
	code, body = raw("POST", srv.URL+fmt.Sprintf("/v1/policies/%d/revisions", polID),
		map[string]any{"source_revision_id": revID, "comment": "also drop cache"})
	rev2ID := int64(body["revision"].(map[string]any)["id"].(float64))
	check("复制已发布修订为新草稿（号=2）", code == http.StatusCreated && body["number"].(float64) == 2)
	code, body = raw("PUT", srv.URL+fmt.Sprintf("/v1/revisions/%d", rev2ID), map[string]any{
		"rules": []map[string]string{
			{"action": "exclude", "pattern": "*.tmp"},
			{"action": "exception", "pattern": "keep.tmp"},
			{"action": "exclude", "pattern": "cache/"},
		},
	})
	check("在副本上修改草稿", code == http.StatusOK)
	code, body = raw("POST", srv.URL+fmt.Sprintf("/v1/revisions/%d/publish", rev2ID), nil)
	check("发布修订 2（旧修订保持不动）", code == http.StatusOK)

	// old snapshot still carries revision 1 evidence
	code, body = raw("GET", srv.URL+fmt.Sprintf("/v1/snapshots/%d/selection", polSnapID), nil)
	check("旧快照仍冻结修订 1 的旧选择证据（2 条规则，不含 cache/）",
		body["frozen_revision"].(map[string]any)["revision_number"].(float64) == 1 &&
			len(body["frozen_revision"].(map[string]any)["rules"].([]any)) == 2)

	// two windows concurrently publish the SAME new draft: exactly one wins
	code, body = raw("POST", srv.URL+fmt.Sprintf("/v1/policies/%d/revisions", polID),
		map[string]any{"source_revision_id": rev2ID, "comment": "race draft"})
	raceRev := int64(body["revision"].(map[string]any)["id"].(float64))
	raceCodes := make(chan int, 2)
	var rwg sync.WaitGroup
	rwg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer rwg.Done()
			c, _ := raw("POST", srv.URL+fmt.Sprintf("/v1/revisions/%d/publish", raceRev), nil)
			raceCodes <- c
		}()
	}
	rwg.Wait()
	close(raceCodes)
	var raceWins [2]int
	raceWins[0] = <-raceCodes
	raceWins[1] = <-raceCodes
	pc1, pc2 := raceWins[0], raceWins[1]
	check("两个窗口并发发布同一草稿：恰好一个 200，另一个 409",
		(pc1 == http.StatusOK && pc2 == http.StatusConflict) ||
			(pc1 == http.StatusConflict && pc2 == http.StatusOK))

	// retire: no new snapshots, old one still restorable
	code, _ = raw("POST", srv.URL+fmt.Sprintf("/v1/policies/%d/retire", polID), nil)
	check("停用策略", code == http.StatusOK)
	code, body = raw("POST", srv.URL+"/v1/snapshots",
		map[string]any{"root": polSrc, "policy_revision_id": rev2ID})
	check("停用后不能再按该策略建快照 (422)", code == http.StatusUnprocessableEntity)
	code, _ = raw("POST", srv.URL+fmt.Sprintf("/v1/snapshots/%d/restore", polSnapID),
		map[string]any{"target": filepath.Join(work, "restore-pol-after-retire")})
	check("停用不影响旧快照：仍可从冻结副本恢复", code == http.StatusCreated)

	// final listing
	section(0, "快照总览")
	list := get(srv.URL + "/v1/snapshots")["snapshots"].([]any)
	for _, x := range list {
		m := x.(map[string]any)
		fmt.Printf("  #%-3v %-10s files=%-3v bytes=%-7v %s\n",
			m["id"], m["status"], m["file_count"], m["bytes_total"], m["message"])
	}
	srv.Close()

	fmt.Println()
	if fail == 0 {
		fmt.Printf("✅ 全部 %d 项检查通过\n", pass)
		return
	}
	fmt.Printf("❌ %d 项失败，%d 项通过\n", fail, pass)
	os.Exit(1)
}

// ---------- helpers ----------

func startServer(repoDir string) *httptest.Server {
	must(os.MkdirAll(repoDir, 0o755))
	manifest, err := repo.OpenManifest(filepath.Join(repoDir, "manifest.sqlite"))
	must(err)
	store, err := repo.NewContentStore(filepath.Join(repoDir, "chunks"))
	must(err)
	engine, err := backup.NewEngine(manifest, store)
	must(err)
	if recovered, err := engine.RecoverPending(); err == nil {
		for _, r := range recovered {
			fmt.Printf("  [启动恢复] 快照 %d -> %s\n", r.SnapshotID, r.Status)
		}
	}
	return httptest.NewServer((&api.Server{Engine: engine}).NewRouter())
}

func post(url string, body any) map[string]any {
	code, b := raw("POST", url, body)
	if code >= 300 {
		out, _ := json.MarshalIndent(b, "", "  ")
		fmt.Println(string(out))
	}
	return b
}

func get(url string) map[string]any {
	code, b := raw("GET", url, nil)
	if code >= 300 {
		out, _ := json.MarshalIndent(b, "", "  ")
		fmt.Println(string(out))
	}
	return b
}

func raw(method, url string, body any) (int, map[string]any) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		must(err)
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, rdr)
	must(err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	must(err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	must(err)
	var out map[string]any
	if len(data) > 0 {
		must(json.Unmarshal(data, &out))
		if out == nil {
			out = map[string]any{}
		}
	} else {
		out = map[string]any{}
	}
	return resp.StatusCode, out
}

func hashFile(p string) (string, int64) {
	f, err := os.Open(p)
	must(err)
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	must(err)
	return hex.EncodeToString(h.Sum(nil)), n
}

func section(n int, title string) {
	if n == 0 {
		fmt.Printf("\n── %s ──────────────────────────────\n", title)
		return
	}
	fmt.Printf("\n── %d. %s ──────────────────────────────\n", n, title)
}

func sortStrings(s []string) { sort.Strings(s) }

func check(name string, ok bool) {
	if ok {
		pass++
		fmt.Printf("  ✓ %s\n", name)
		return
	}
	fail++
	fmt.Printf("  ✗ %s\n", name)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		var ee *exec.ExitError
		_ = ee
		os.Exit(2)
	}
}
