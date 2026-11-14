package task

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linrong007/Sunshine/internal/hook"
)

// AddSessionOptions 是 add-session 的参数（对应 add_session.py 的 flags）。
type AddSessionOptions struct {
	Title          string
	Commit         string // 逗号分隔的 OID，或 "-"（无提交）
	Summary        string
	Branch         string
	Package        string
	CommitSubjects []string // OID=主题（可重复）
	Changes        []string
	Tests          []string
	NextSteps      []string
	// ExtraContent 是 --content-file / --stdin 读到的自由内容（CLI 层读入）。
	ExtraContent   string
	IdempotencyKey string
	NoCommit       bool
}

var (
	commitTokenRe    = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
	idempotencyKeyRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	sessionHeadingRe = regexp.MustCompile(`(?m)^## (?:会话|Session) (\d+)[：:]`)
	legacyMarkerRe   = regexp.MustCompile(`^<!-- (?:sunshine|trellis)-session: fp=([0-9a-f]{16}) -->$`)
	entryDateRe      = regexp.MustCompile(`^\*\*日期\*\*: (\d{4}-\d{2}-\d{2})\s*$`)
	entryDateENRe    = regexp.MustCompile(`^\*\*Date\*\*: (\d{4}-\d{2}-\d{2})\s*$`)
)

const (
	markerPrefix  = "<!-- sunshine-session:"
	markerVersion = 2
)

// AddSession 往开发者 journal 追加会话记录并更新 index.md（对应 add_session.py）。
func (r *Runner) AddSession(opts AddSessionOptions) int {
	if !r.requireRoot() {
		return 1
	}
	if strings.TrimSpace(opts.Title) == "" {
		r.errf("--title 必填")
		return 1
	}
	// ---- 预检：任何写入之前全部通过（commit 证据解析失败时不写任何文件） ----
	if opts.IdempotencyKey != "" && !idempotencyKeyRe.MatchString(opts.IdempotencyKey) {
		r.errf("--idempotency-key %q 无效：应为 1-64 个 [A-Za-z0-9._-] 字符", opts.IdempotencyKey)
		return 1
	}
	tokens, err := parseCommitTokens(opts.Commit)
	if err != nil {
		r.errf("%v", err)
		return 1
	}
	overrides, err := parseSubjectOverrides(opts.CommitSubjects)
	if err != nil {
		r.errf("%v", err)
		return 1
	}
	evidence, err := buildCommitEvidence(r.Root, tokens, overrides)
	if err != nil {
		r.errf("%v", err)
		return 1
	}
	extras := LoadConfigExtras(r.Root)
	if extras.SessionAutoCommit && IsLinkedWorktree(r.Root) {
		fmt.Fprintln(r.errOut(), "[提示] 当前在 git worktree 里且 session_auto_commit 已开启：journal-*.md 会按 .gitattributes 自动合并，但 index.md 在并行 worktree/分支间的冲突是预期内的，任选一边解决即可（任务状态在 task.json，不在 index.md）。")
	}
	dev := Developer(r.Root)
	if dev == "" {
		// 尝试从 git user.name 补开发者身份（对应 ensure_developer 的
		// 初始化路径）；补不上则报错。
		if name := gitUserName(r.Root); name != "" {
			dev = name
			_ = initDeveloperWorkspace(r.Root, dev, r.today())
		}
	}
	if dev == "" {
		r.errf("未设置开发者。请先运行 sunshine init。")
		fmt.Fprintln(r.errOut(), DeveloperHint)
		return 1
	}
	devDir := filepath.Join(r.Root, DirSunshine, "workspace", dev)
	if st, err := os.Stat(devDir); err != nil || !st.IsDir() {
		// 工作区目录缺失时按 init_developer 的模板补建。
		if err := initDeveloperWorkspace(r.Root, dev, r.today()); err != nil {
			r.errf("工作区目录不存在：%s", devDir)
			return 1
		}
	}
	// 当前任务（供 package 与分支推断）。
	active := ResolveActiveTask(r.Root, "current", "")
	var taskBranch, taskPackage string
	if active.TaskPath != "" {
		if info, err := loadTaskInfo(filepath.Join(r.Root, active.TaskPath)); err == nil && info != nil {
			taskBranch = info.Raw.getStringField("branch")
			taskPackage = info.Package
		}
	}
	// package：显式 > 任务包 > default_package；单仓忽略。
	cfg := LoadConfig(r.Root)
	packageName := ""
	if opts.Package != "" {
		if !cfg.IsMonorepo() {
			r.warnf("单仓项目忽略 --package")
		} else if !cfg.Packages[opts.Package] {
			var available []string
			for name := range cfg.Packages {
				available = append(available, name)
			}
			sort.Strings(available)
			r.errf("未知包 %q，可用：%s", opts.Package, strings.Join(available, ", "))
			return 1
		} else {
			packageName = opts.Package
		}
	} else if cfg.IsMonorepo() {
		packageName = resolvePackage(taskPackage, cfg, r.errOut())
	}
	branch := resolveSessionBranch(r.Root, opts.Branch, taskBranch, r.errOut())
	summary := opts.Summary
	if summary == "" {
		summary = "未提供会话摘要。"
	}
	maxLines := cfg.MaxJournalLines
	if maxLines <= 0 {
		maxLines = 2000
	}
	indexFile := filepath.Join(devDir, "index.md")
	today := r.today()
	payload := fingerprintPayload(dev, opts.Title, summary, packageName, branch, evidence, opts.Changes, opts.ExtraContent, opts.Tests, opts.NextSteps, opts.IdempotencyKey)
	marker := renderMarker(computeRecordFingerprint(payload))
	marker, err = resolveEffectiveMarker(devDir, payload, marker)
	if err != nil {
		r.errf("%v", err)
		return 1
	}
	state, matchedFile, matchedNum, classifyErr := classifyRecord(r.Root, devDir, indexFile, marker)
	if classifyErr != "" {
		r.errf("%s", classifyErr)
		return 1
	}
	if state == stateCommitted && opts.IdempotencyKey != "" {
		matchedName := "journal"
		if matchedFile != "" {
			matchedName = filepath.Base(matchedFile)
		}
		fmt.Fprintf(r.errOut(), "[OK] 幂等键 %q 对应的会话 %d 已记录并提交（%s），无需操作。\n", opts.IdempotencyKey, matchedNum, matchedName)
		return 0
	}
	// 已提交的记录不再复用：步进 generation，生成新 marker。
	generation := 0
	for state == stateCommitted {
		generation++
		payloadWithGen := map[string]any{}
		for k, v := range payload {
			payloadWithGen[k] = v
		}
		payloadWithGen["generation"] = generation
		marker = renderMarker(computeRecordFingerprint(payloadWithGen))
		state, matchedFile, matchedNum, classifyErr = classifyRecord(r.Root, devDir, indexFile, marker)
		if classifyErr != "" {
			r.errf("%s", classifyErr)
			return 1
		}
	}
	fmt.Fprintln(r.errOut(), "========================================")
	fmt.Fprintln(r.errOut(), "添加会话")
	fmt.Fprintln(r.errOut(), "========================================")
	fmt.Fprintln(r.errOut(), "")
	// ---- Absent → journal-recorded ----
	var targetFile string
	var targetNum int
	var newSession int
	if state == stateAbsent {
		journalFile, currentNum, currentLines := getLatestJournalInfo(devDir)
		newSession = resolveNextSession(r.Root, devDir, indexFile)
		content := generateSessionContent(newSession, opts.Title, evidence, summary, today, marker, packageName, branch, opts.Changes, opts.ExtraContent, opts.Tests, opts.NextSteps)
		contentLines := len(strings.Split(content, "\n"))
		fmt.Fprintf(r.errOut(), "会话：%d\n标题：%s\n提交：%s\n\n", newSession, opts.Title, formatCommitDisplay(evidence))
		fmt.Fprintf(r.errOut(), "当前日志文件：journal-%d.md\n当前行数：%d\n新增行数：%d\n追加后总计：%d\n\n", currentNum, currentLines, contentLines, currentLines+contentLines)
		targetFile = journalFile
		targetNum = currentNum
		if currentLines+contentLines > maxLines {
			targetNum = currentNum + 1
			fmt.Fprintf(r.errOut(), "[!] 超过 %d 行，新建 journal-%d.md\n", maxLines, targetNum)
			created, err := createNewJournalFile(devDir, targetNum, dev, today, maxLines)
			if err != nil {
				r.errf("创建 journal-%d.md 失败：%v；未记录任何内容。", targetNum, err)
				return 1
			}
			targetFile = created
			fmt.Fprintf(r.errOut(), "已创建：%s\n", targetFile)
		}
		if targetFile == "" {
			r.errf("没有可追加的日志文件。请先运行 sunshine init 初始化开发者。")
			return 1
		}
		existing := ""
		if data, err := os.ReadFile(targetFile); err == nil {
			existing = string(data)
		}
		if err := writeFileAtomic(targetFile, []byte(existing+content), 0o644); err != nil {
			r.errf("向 %s 追加会话失败：%v；原日志内容未动，未记录任何内容。", filepath.Base(targetFile), err)
			return 1
		}
		fmt.Fprintf(r.errOut(), "[OK] 会话已追加到 %s\n", filepath.Base(targetFile))
		state = stateJournalRecorded
	} else {
		if matchedFile == "" || matchedNum == 0 {
			r.errf("恢复状态 %q 但没有匹配的日志条目。", state)
			return 1
		}
		targetFile = matchedFile
		newSession = matchedNum
		targetNum = extractJournalNum(filepath.Base(targetFile))
		fmt.Fprintf(r.errOut(), "[恢复] 会话 %d 已在 %s 且未提交，从 %q 继续，不再追加第二条。\n\n", newSession, filepath.Base(targetFile), state)
	}
	fmt.Fprintln(r.errOut(), "")
	// ---- Journal-recorded → index-recorded ----
	activeFile := fmt.Sprintf("journal-%d.md", targetNum)
	if state == stateJournalRecorded {
		if !updateIndex(indexFile, devDir, opts.Title, evidence, newSession, activeFile, today, branch, r.errOut()) {
			fmt.Fprintf(r.errOut(), "[受阻] 检查点：会话 %d 已在 %s，但 index.md 未更新。请修复 index.md 后重新运行完全相同的命令，它会补行而不追加第二条会话。\n", newSession, activeFile)
			return 1
		}
		state = stateIndexRecorded
	} else {
		fmt.Fprintf(r.errOut(), "[OK] index.md 已记录会话 %d。\n", newSession)
	}
	fmt.Fprintln(r.errOut(), "")
	fmt.Fprintln(r.errOut(), "========================================")
	fmt.Fprintf(r.errOut(), "[OK] 会话 %d 添加成功！\n", newSession)
	fmt.Fprintln(r.errOut(), "========================================")
	fmt.Fprintln(r.errOut(), "")
	fmt.Fprintln(r.errOut(), "更新的文件：")
	fmt.Fprintf(r.errOut(), "  - %s\n", filepath.Base(targetFile))
	fmt.Fprintln(r.errOut(), "  - index.md")
	// ---- Index-recorded → committed ----
	if opts.NoCommit {
		return 0
	}
	_ = EnsureGitattributes(r.Root)
	fmt.Fprintln(r.errOut(), "")
	outcome := autoCommitWorkspace(r.Root, r.errOut())
	switch outcome {
	case commitFailed:
		printCommitCheckpoint(newSession, filepath.Base(targetFile), r.errOut())
		return 1
	case commitDone:
		// 校验 HEAD 里的 journal 确实含本条 marker。
		rel := ""
		if rp, err := filepath.Rel(r.Root, targetFile); err == nil {
			rel = filepath.ToSlash(rp)
		}
		if content, ok := ContentAtHead(r.Root, rel); !ok || !strings.Contains(content, marker) {
			fmt.Fprintf(r.errOut(), "[警告] 自动提交报告成功，但 HEAD 里的 %s 不含本次会话记录。\n", filepath.Base(targetFile))
			printCommitCheckpoint(newSession, filepath.Base(targetFile), r.errOut())
			return 1
		}
	}
	return 0
}

func gitUserName(root string) string {
	code, out, _ := runGit(root, 15*time.Second, "config", "user.name")
	if code != 0 {
		return ""
	}
	return strings.TrimSpace(out)
}

// ---------------------------------------------------------------------------
// 开发者工作区初始化（对应 developer.init_developer，中文模板）
// ---------------------------------------------------------------------------

func initDeveloperWorkspace(root, name, today string) error {
	devFile := filepath.Join(root, DirSunshine, ".developer")
	wsDir := filepath.Join(root, DirSunshine, "workspace", name)
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return err
	}
	initializedAt := time.Now().Format(time.RFC3339)
	if _, err := os.Stat(devFile); os.IsNotExist(err) {
		if err := writeFileAtomic(devFile, []byte("name="+name+"\ninitialized_at="+initializedAt+"\n"), 0o644); err != nil {
			return err
		}
	}
	journalFile := filepath.Join(wsDir, "journal-1.md")
	if _, err := os.Stat(journalFile); os.IsNotExist(err) {
		content := "# 开发日志 - " + name + "（第 1 部分）\n\n> AI 开发会话日志\n> 开始：" + today + "\n\n---\n\n"
		if err := writeFileAtomic(journalFile, []byte(content), 0o644); err != nil {
			return err
		}
	}
	indexFile := filepath.Join(wsDir, "index.md")
	if _, err := os.Stat(indexFile); os.IsNotExist(err) {
		content := devIndexTemplate(name)
		if err := writeFileAtomic(indexFile, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func devIndexTemplate(name string) string {
	return `# 工作区索引 - ` + name + `

> AI 开发会话的日志记录。

---

## 当前状态

<!-- @@@auto:current-status -->
- **当前文件**：` + "`journal-1.md`" + `
- **总会话数**：0
- **最近活动**：-
<!-- @@@/auto:current-status -->

---

## 活动文档

<!-- @@@auto:active-documents -->
| 文件 | 行数 | 状态 |
|------|-------|--------|
| ` + "`journal-1.md`" + ` | ~0 | 活动中 |
<!-- @@@/auto:active-documents -->

---

## 会话历史

<!-- @@@auto:session-history -->
| # | 日期 | 标题 | 提交 | 分支 |
|---|------|-------|---------|--------|
<!-- @@@/auto:session-history -->

---

## 说明

- 会话追加到 journal 文件
- 当前文件超过 2000 行时新建 journal 文件
- 用 ` + "`sunshine task add-session`" + ` 记录会话
`
}

// ---------------------------------------------------------------------------
// 提交证据（R1）：本地解析不到的 OID 必须配 --commit-subject，否则
// 在写任何文件前失败。
// ---------------------------------------------------------------------------

func parseCommitTokens(commit string) ([]string, error) {
	raw := strings.TrimSpace(commit)
	if raw == "" || raw == "-" {
		return []string{}, nil
	}
	var tokens []string
	for _, part := range strings.Split(raw, ",") {
		token := strings.TrimSpace(part)
		if token == "" {
			continue
		}
		if !commitTokenRe.MatchString(token) {
			return nil, fmt.Errorf("--commit 里的 %q 无效：应为 7-40 位十六进制提交 OID，或用 - 表示无提交的规划会话", token)
		}
		found := false
		for _, t := range tokens {
			if t == token {
				found = true
			}
		}
		if !found {
			tokens = append(tokens, token)
		}
	}
	return tokens, nil
}

func parseSubjectOverrides(values []string) (map[string]string, error) {
	overrides := map[string]string{}
	for _, raw := range values {
		oidPart, subject, ok := strings.Cut(raw, "=")
		if !ok {
			return nil, fmt.Errorf("--commit-subject %q 无效：应为 <oid>=<主题>", raw)
		}
		oid := strings.ToLower(strings.TrimSpace(oidPart))
		subject = strings.TrimSpace(subject)
		if !commitTokenRe.MatchString(oid) {
			return nil, fmt.Errorf("--commit-subject 的键 %q 无效：应为 7-40 位十六进制提交 OID", strings.TrimSpace(oidPart))
		}
		if subject == "" {
			return nil, fmt.Errorf("--commit-subject %q 的主题为空", oid)
		}
		if len(subject) > 500 {
			return nil, fmt.Errorf("--commit-subject %q 的主题超过 500 字符", oid)
		}
		if _, dup := overrides[oid]; dup {
			return nil, fmt.Errorf("--commit-subject 里 %q 重复", oid)
		}
		overrides[oid] = subject
	}
	return overrides, nil
}

func buildCommitEvidence(root string, tokens []string, overrides map[string]string) ([][2]string, error) {
	var evidence [][2]string
	var unresolved []string
	for _, oid := range tokens {
		if subject, ok := overrides[strings.ToLower(oid)]; ok {
			evidence = append(evidence, [2]string{oid, subject})
			continue
		}
		if subject := ResolveCommitSubject(root, oid); subject != "" {
			evidence = append(evidence, [2]string{oid, subject})
			continue
		}
		unresolved = append(unresolved, oid)
	}
	if len(unresolved) > 0 {
		return nil, fmt.Errorf("无法解析以下提交的证据：%s。请先 fetch 对象、纠正 OID，或用 --commit-subject <oid>=<主题> 指定。未写入任何内容。", strings.Join(unresolved, ", "))
	}
	var unused []string
	tokenSet := map[string]bool{}
	for _, t := range tokens {
		tokenSet[strings.ToLower(t)] = true
	}
	for oid := range overrides {
		if !tokenSet[oid] {
			unused = append(unused, oid)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		return nil, fmt.Errorf("--commit-subject 给了不在 --commit 里的 OID：%s。映射必须一一对应。未写入任何内容。", strings.Join(unused, ", "))
	}
	return evidence, nil
}

// ---------------------------------------------------------------------------
// 指纹（R2）：v2 marker，date-free；v1（legacy）只读不写。
// ---------------------------------------------------------------------------

func normalizeText(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func fingerprintPayload(developer, title, summary, pkg, branch string, evidence [][2]string, changes []string, extra string, tests, nextSteps []string, idempotencyKey string) map[string]any {
	commits := []any{}
	for _, e := range evidence {
		commits = append(commits, []string{e[0], normalizeText(e[1])})
	}
	normList := func(items []string) []any {
		out := []any{}
		for _, item := range items {
			out = append(out, normalizeText(item))
		}
		return out
	}
	return map[string]any{
		"developer":       developer,
		"title":           normalizeText(title),
		"summary":         normalizeText(summary),
		"package":         pkg,
		"branch":          branch,
		"commits":         commits,
		"changes":         normList(changes),
		"extra":           normalizeText(extra),
		"tests":           normList(tests),
		"next_steps":      normList(nextSteps),
		"idempotency_key": idempotencyKey,
	}
}

func computeRecordFingerprint(payload map[string]any) string {
	encoded, _ := json.Marshal(payload) // map 键按字母序，等价于 sort_keys=True
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])[:16]
}

func computeLegacyFingerprint(payload map[string]any, date string) string {
	withDate := map[string]any{}
	for k, v := range payload {
		withDate[k] = v
	}
	withDate["date"] = date
	return computeRecordFingerprint(withDate)
}

func renderMarker(fingerprint string) string {
	return fmt.Sprintf("%s v=%d fp=%s -->", markerPrefix, markerVersion, fingerprint)
}

// ---------------------------------------------------------------------------
// 分支解析：--branch > task.json 的 branch（须在本地存在）> 当前检出分支。
// ---------------------------------------------------------------------------

func resolveSessionBranch(root, cliBranch, taskBranch string, errOut io.Writer) string {
	if cliBranch != "" {
		return cliBranch
	}
	current := CurrentBranch(root)
	taskBranch = strings.TrimSpace(taskBranch)
	if taskBranch == "" {
		if current != "" {
			return current
		}
		return ""
	}
	if BranchRefExists(root, taskBranch) {
		return taskBranch
	}
	if current != "" {
		fmt.Fprintf(errOut, "警告：task.json 的分支 %q 在本地与 origin/%s 都不存在，改用当前分支 %q。\n", taskBranch, taskBranch, current)
		return current
	}
	fmt.Fprintf(errOut, "警告：task.json 的分支 %q 在本地与 origin/%s 都不存在，已省略分支。\n", taskBranch, taskBranch)
	return ""
}

// resolvePackage 按任务包 → default_package 推断（非法推断值警告后跳过）。
func resolvePackage(taskPackage string, cfg hook.Config, errOut io.Writer) string {
	if taskPackage != "" {
		if cfg.Packages[taskPackage] {
			return taskPackage
		}
		fmt.Fprintf(errOut, "警告：当前任务的包 %q 不在 config.yaml 的 packages 里，已忽略。\n", taskPackage)
	}
	if cfg.DefaultPackage != "" {
		if cfg.Packages[cfg.DefaultPackage] {
			return cfg.DefaultPackage
		}
		fmt.Fprintf(errOut, "警告：default_package %q 不在 packages 里，已忽略。\n", cfg.DefaultPackage)
	}
	return ""
}

// ---------------------------------------------------------------------------
// 会话编号（跨分支收敛）
// ---------------------------------------------------------------------------

func getLatestJournalInfo(devDir string) (string, int, int) {
	entries, err := os.ReadDir(devDir)
	if err != nil {
		return "", 0, 0
	}
	latestFile := ""
	latestNum := -1
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "journal-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		num := extractJournalNum(e.Name())
		if num > latestNum {
			latestNum = num
			latestFile = filepath.Join(devDir, e.Name())
		}
	}
	if latestFile != "" {
		return latestFile, latestNum, countLines(latestFile)
	}
	return "", 0, 0
}

func extractJournalNum(filename string) int {
	re := regexp.MustCompile(`(\d+)`)
	m := re.FindStringSubmatch(filename)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func getCurrentSession(indexFile string) int {
	data, err := os.ReadFile(indexFile)
	if err != nil {
		return 0
	}
	return maxSessionInIndex(string(data))
}

func maxSessionInIndex(content string) int {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "总会话数") || strings.Contains(line, "Total Sessions") {
			if m := regexp.MustCompile(`[：:]\s*(\d+)`).FindStringSubmatch(line); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

func maxSessionInJournal(content string) int {
	max := 0
	for _, m := range sessionHeadingRe.FindAllStringSubmatch(content, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	return max
}

func maxLocalSession(devDir, indexFile string) int {
	highest := getCurrentSession(indexFile)
	entries, _ := os.ReadDir(devDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "journal-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(devDir, e.Name()))
		if err != nil {
			continue
		}
		if n := maxSessionInJournal(string(data)); n > highest {
			highest = n
		}
	}
	return highest
}

func defaultBranchLocal(root string) string { return DefaultBranchLocal(root) }

func convergenceRefs(root string) []string {
	var refs []string
	add := func(ref string) {
		for _, r := range refs {
			if r == ref {
				return
			}
		}
		refs = append(refs, ref)
	}
	if code, _, _ := runGit(root, 15*time.Second, "rev-parse", "--verify", "--quiet", "HEAD"); code == 0 {
		add("HEAD")
	}
	if def := defaultBranchLocal(root); def != "" {
		for _, ref := range []string{"refs/heads/" + def, "refs/remotes/origin/" + def} {
			if code, _, _ := runGit(root, 15*time.Second, "show-ref", "--verify", "--quiet", ref); code == 0 {
				add(ref)
			}
		}
	}
	for _, pattern := range []string{"refs/heads/", "refs/remotes/"} {
		code, out, _ := runGit(root, 15*time.Second, "for-each-ref", "--format=%(refname)", pattern)
		if code != 0 {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			ref := strings.TrimSpace(line)
			if ref != "" && !strings.HasSuffix(ref, "/HEAD") {
				add(ref)
			}
		}
	}
	if len(refs) > 100 {
		refs = refs[:100]
	}
	return refs
}

func maxSessionAcrossRefs(root string, refs []string, devRel string) int {
	if len(refs) == 0 {
		return 0
	}
	args := []string{"grep", "--no-color", "-h", "-E",
		"-e", `^## 会话 [0-9]+：`,
		"-e", `^## Session [0-9]+:`,
		"-e", `总会话数`,
		"-e", `Total Sessions`,
	}
	args = append(args, refs...)
	args = append(args, "--", devRel)
	code, out, _ := runGit(root, 15*time.Second, args...)
	if code > 1 || out == "" {
		return 0
	}
	max := 0
	for _, m := range regexp.MustCompile(`## (?:会话|Session) (\d+)[：:]`).FindAllStringSubmatch(out, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	for _, m := range regexp.MustCompile(`(?:总会话数|Total Sessions)[^\d]{0,8}(\d+)`).FindAllStringSubmatch(out, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	return max
}

func resolveNextSession(root, devDir, indexFile string) int {
	highest := maxLocalSession(devDir, indexFile)
	if rel, err := filepath.Rel(root, devDir); err == nil {
		if n := maxSessionAcrossRefs(root, convergenceRefs(root), filepath.ToSlash(rel)); n > highest {
			highest = n
		}
	}
	return highest + 1
}

// ---------------------------------------------------------------------------
// 待记录分类（R3/R4）
// ---------------------------------------------------------------------------

const (
	stateAbsent          = "absent"
	stateJournalRecorded = "journal-recorded"
	stateIndexRecorded   = "index-recorded"
	stateCommitted       = "committed"
)

func findMarkerEntries(devDir, marker string) []struct {
	file   string
	num    int // 0 表示 marker 上方 3 行内无会话标题（畸形）
	hasNum bool
} {
	var hits []struct {
		file   string
		num    int
		hasNum bool
	}
	entries, _ := os.ReadDir(devDir)
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "journal-") && strings.HasSuffix(e.Name(), ".md") {
			files = append(files, filepath.Join(devDir, e.Name()))
		}
	}
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if strings.TrimSpace(line) != marker {
				continue
			}
			num := 0
			hasNum := false
			for j := i - 1; j >= 0 && j >= i-4; j-- {
				if m := sessionHeadingRe.FindStringSubmatch(lines[j]); m != nil {
					num, _ = strconv.Atoi(m[1])
					hasNum = true
					break
				}
			}
			hits = append(hits, struct {
				file   string
				num    int
				hasNum bool
			}{f, num, hasNum})
		}
	}
	return hits
}

func entryDateAt(lines []string, markerIndex int) string {
	for _, line := range lines[markerIndex+1 : min(markerIndex+6, len(lines))] {
		if m := entryDateRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return m[1]
		}
		if m := entryDateENRe.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return m[1]
		}
	}
	return ""
}

// resolveEffectiveMarker：v2 marker 优先；否则找 v1 legacy marker
// （用条目自身的日期重算指纹比对）。
func resolveEffectiveMarker(devDir string, payload map[string]any, marker string) (string, error) {
	if len(findMarkerEntries(devDir, marker)) > 0 {
		return marker, nil
	}
	matches := map[string]bool{}
	entries, _ := os.ReadDir(devDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "journal-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(devDir, e.Name()))
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			m := legacyMarkerRe.FindStringSubmatch(strings.TrimSpace(line))
			if m == nil {
				continue
			}
			date := entryDateAt(lines, i)
			if date == "" {
				continue
			}
			if computeLegacyFingerprint(payload, date) == m[1] {
				matches[strings.TrimSpace(line)] = true
			}
		}
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("发现 %d 条待处理日志条目使用旧版 marker，无法判断恢复哪一条——请删除重复条目，或传 --idempotency-key 记录新会话", len(matches))
	}
	for m := range matches {
		return m, nil
	}
	return marker, nil
}

func indexHasSessionRow(indexFile string, sessionNum int) bool {
	data, err := os.ReadFile(indexFile)
	if err != nil {
		return false
	}
	rowRe := regexp.MustCompile(fmt.Sprintf(`^\|\s*%d\s*\|`, sessionNum))
	inHistory := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "@@@auto:session-history") {
			inHistory = true
			continue
		}
		if strings.Contains(line, "@@@/auto:session-history") {
			inHistory = false
			continue
		}
		if inHistory && rowRe.MatchString(line) {
			return true
		}
	}
	return false
}

// classifyRecord 分类当前记录的状态。
func classifyRecord(root, devDir, indexFile, marker string) (state, matchedFile string, matchedNum int, errMsg string) {
	hits := findMarkerEntries(devDir, marker)
	if len(hits) == 0 {
		return stateAbsent, "", 0, ""
	}
	if len(hits) > 1 {
		var files []string
		seen := map[string]bool{}
		for _, h := range hits {
			name := filepath.Base(h.file)
			if !seen[name] {
				seen[name] = true
				files = append(files, name)
			}
		}
		sort.Strings(files)
		return "", "", 0, fmt.Sprintf("发现 %d 条日志携带本记录的 marker（%s），无法判断恢复哪一条——请删除重复条目，或传 --idempotency-key 记录新会话", len(hits), strings.Join(files, ", "))
	}
	hit := hits[0]
	if !hit.hasNum {
		return "", "", 0, fmt.Sprintf("%s 携带本记录的 marker，但上方没有“## 会话 N：”标题，待处理记录已损坏；请手工修复后再重试", filepath.Base(hit.file))
	}
	rel, err := filepath.Rel(root, hit.file)
	if err == nil {
		if content, ok := ContentAtHead(root, filepath.ToSlash(rel)); ok && strings.Contains(content, marker) {
			return stateCommitted, hit.file, hit.num, ""
		}
	}
	if indexHasSessionRow(indexFile, hit.num) {
		return stateIndexRecorded, hit.file, hit.num, ""
	}
	if recordedTotal := getCurrentSession(indexFile); recordedTotal > hit.num {
		return "", "", 0, fmt.Sprintf("会话 %d 的日志条目没有 index 行，但 index.md 已计 %d 个会话；补行会改写更晚的记录，请手工处理 index.md", hit.num, recordedTotal)
	}
	return stateJournalRecorded, hit.file, hit.num, ""
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

func renderBulletSection(header string, items []string, bulletPrefix string) string {
	if len(items) == 0 {
		return ""
	}
	var bullets []string
	for _, item := range items {
		bullets = append(bullets, bulletPrefix+item)
	}
	return "\n\n### " + header + "\n\n" + strings.Join(bullets, "\n")
}

func escapeMarkdownCell(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	collapsed = strings.ReplaceAll(collapsed, "\\", "\\\\")
	return strings.ReplaceAll(collapsed, "|", "\\|")
}

func formatCommitDisplay(evidence [][2]string) string {
	if len(evidence) == 0 {
		return "-"
	}
	var parts []string
	for _, e := range evidence {
		parts = append(parts, "`"+e[0]+"`")
	}
	return strings.Join(parts, ", ")
}

func generateSessionContent(sessionNum int, title string, evidence [][2]string, summary, today, marker, pkg, branch string, changes []string, extraContent string, tests, nextSteps []string) string {
	var commitTable string
	if len(evidence) > 0 {
		commitTable = "| 哈希 | 说明 |\n|------|---------|"
		for _, e := range evidence {
			commitTable += "\n| `" + e[0] + "` | " + escapeMarkdownCell(e[1]) + " |"
		}
	} else {
		commitTable = "（无提交——规划会话）"
	}
	packageLine := ""
	if pkg != "" {
		packageLine = "\n**包**：" + pkg
	}
	branchLine := ""
	if branch != "" {
		branchLine = "\n**分支**：`" + branch + "`"
	}
	mainChanges := ""
	if len(changes) > 0 {
		mainChanges = renderBulletSection("主要变更", changes, "- ")
	} else if extraContent != "" {
		mainChanges = "\n\n### 主要变更\n\n" + extraContent
	}
	testingSection := renderBulletSection("测试", tests, "- [OK] ")
	nextStepsSection := renderBulletSection("下一步", nextSteps, "- ")
	return fmt.Sprintf("\n\n## 会话 %d：%s\n%s\n\n**日期**：%s\n**任务**：%s%s%s\n\n### 摘要\n\n%s%s\n\n### Git 提交\n\n%s%s\n\n### 状态\n\n[OK] **已完成**%s\n",
		sessionNum, title, marker, today, title, packageLine, branchLine,
		summary, mainChanges, commitTable, testingSection, nextStepsSection)
}

func createNewJournalFile(devDir string, num int, developer, today string, maxLines int) (string, error) {
	prevNum := num - 1
	newFile := filepath.Join(devDir, fmt.Sprintf("journal-%d.md", num))
	content := fmt.Sprintf("# 开发日志 - %s（第 %d 部分）\n\n> 接续 `journal-%d.md`（约 %d 行时归档）\n> 开始：%s\n\n---\n\n",
		developer, num, prevNum, maxLines, today)
	if err := writeFileAtomic(newFile, []byte(content), 0o644); err != nil {
		return "", err
	}
	return newFile, nil
}

func countJournalFiles(devDir string, activeNum int) string {
	entries, _ := os.ReadDir(devDir)
	type jf struct {
		name  string
		num   int
		lines int
	}
	var files []jf
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "journal-") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		files = append(files, jf{e.Name(), extractJournalNum(e.Name()), countLines(filepath.Join(devDir, e.Name()))})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].num > files[j].num })
	var rows []string
	for _, f := range files {
		status := "已归档"
		if f.name == fmt.Sprintf("journal-%d.md", activeNum) {
			status = "活动中"
		}
		rows = append(rows, fmt.Sprintf("| `%s` | ~%d | %s |", f.name, f.lines, status))
	}
	return strings.Join(rows, "\n")
}

// updateIndex 更新开发者 index.md 的三个自动区块。
func updateIndex(indexFile, devDir, title string, evidence [][2]string, newSession int, activeFile, today, branch string, errOut io.Writer) bool {
	commitDisplay := formatCommitDisplay(evidence)
	activeNum := extractJournalNum(activeFile)
	filesTable := countJournalFiles(devDir, activeNum)
	fmt.Fprintf(errOut, "正在更新 index.md（会话 %d）……\n  标题：%s\n  提交：%s\n  当前文件：%s\n\n", newSession, title, commitDisplay, activeFile)
	data, err := os.ReadFile(indexFile)
	if err != nil {
		fmt.Fprintf(errOut, "错误：读 index.md 失败：%v\n", err)
		return false
	}
	content := string(data)
	if !strings.Contains(content, "@@@auto:current-status") {
		fmt.Fprintln(errOut, "错误：index.md 里找不到自动区块标记，请确认标记完整。")
		return false
	}
	lines := strings.Split(content, "\n")
	var out []string
	inStatus, inDocs, inHistory := false, false, false
	headerWritten := false
	branchCell := "`" + branch + "`"
	if branch == "" {
		branchCell = "`-`"
	}
	for _, line := range lines {
		switch {
		case strings.Contains(line, "@@@auto:current-status"):
			out = append(out, line)
			inStatus = true
			out = append(out,
				"- **当前文件**：`"+activeFile+"`",
				fmt.Sprintf("- **总会话数**：%d", newSession),
				"- **最近活动**："+today)
			continue
		case strings.Contains(line, "@@@/auto:current-status"):
			inStatus = false
			out = append(out, line)
			continue
		case strings.Contains(line, "@@@auto:active-documents"):
			out = append(out, line)
			inDocs = true
			out = append(out, "| 文件 | 行数 | 状态 |", "|------|-------|--------|", filesTable)
			continue
		case strings.Contains(line, "@@@/auto:active-documents"):
			inDocs = false
			out = append(out, line)
			continue
		case strings.Contains(line, "@@@auto:session-history"):
			out = append(out, line)
			inHistory = true
			headerWritten = false
			continue
		case strings.Contains(line, "@@@/auto:session-history"):
			inHistory = false
			out = append(out, line)
			continue
		}
		if inStatus || inDocs {
			continue
		}
		if inHistory {
			if isHistoryHeader(line) {
				out = append(out, "| # | 日期 | 标题 | 提交 | 分支 |")
				continue
			}
			if regexp.MustCompile(`^\|[-| ]+\|\s*$`).MatchString(line) && !headerWritten {
				out = append(out, "|---|------|-------|---------|--------|")
				out = append(out, fmt.Sprintf("| %d | %s | %s | %s | %s |", newSession, today, title, commitDisplay, branchCell))
				headerWritten = true
				continue
			}
			out = append(out, line)
			continue
		}
		out = append(out, line)
	}
	if err := writeFileAtomic(indexFile, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		fmt.Fprintf(errOut, "错误：写入 %s 失败：%v\n", indexFile, err)
		return false
	}
	fmt.Fprintln(errOut, "[OK] index.md 已更新！")
	return true
}

func isHistoryHeader(line string) bool {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "|") {
		return false
	}
	// 旧的 4/5/6 列英文表头与中文表头都归一化为 5 列中文表头。
	if strings.Contains(trimmed, "Title") || strings.Contains(trimmed, "标题") {
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// 自动提交
// ---------------------------------------------------------------------------

const (
	commitDone    = "committed"
	commitSkipped = "skipped"
	commitBlocked = "blocked"
	commitFailed  = "failed"
)

func autoCommitWorkspace(root string, errOut io.Writer) string {
	extras := LoadConfigExtras(root)
	if !extras.SessionAutoCommit {
		fmt.Fprintln(errOut, "[OK] session_auto_commit: false——跳过 git 提交。")
		return commitSkipped
	}
	if !IsGitWorktree(root) {
		fmt.Fprintln(errOut, "[警告] 不是 git 仓库——journal/index 已写入，跳过自动提交。")
		return commitBlocked
	}
	if missing := gitIdentityMissing(root); len(missing) > 0 {
		printGitIdentityWarning(missing, errOut)
		return commitBlocked
	}
	commitMsg := extras.SessionCommitMessage
	currentTaskName := ""
	if active := ResolveActiveTask(root, "current", ""); active.TaskPath != "" {
		currentTaskName = filepath.Base(active.TaskPath)
	}
	paths := safeTrellisPathsToAdd(root, currentTaskName)
	if currentTaskName == "" {
		// 无当前任务时不兜底扫描全部任务目录（#303）。
		var filtered []string
		for _, p := range paths {
			if !strings.HasPrefix(p, DirSunshine+"/"+DirTasks+"/") {
				filtered = append(filtered, p)
			}
		}
		paths = filtered
	}
	if len(paths) == 0 {
		fmt.Fprintln(errOut, "[OK] 没有需要提交的工作区变更。")
		return commitSkipped
	}
	ok, errStr := safeGitAdd(root, paths, false)
	if !ok {
		if strings.Contains(strings.ToLower(errStr), "ignored by") {
			if f, ok := errOut.(*os.File); ok {
				printGitignoreWarning(paths, f)
			} else {
				fmt.Fprintf(errOut, "[警告] .gitignore 拦截了 .sunshine/ 下的变更，自动提交已跳过。\n[警告] 若需提交，请检查 .gitignore，不要 `git add -f .sunshine/`。\n")
			}
			return commitBlocked
		}
		fmt.Fprintf(errOut, "[警告] git add 失败：%s\n", strings.TrimSpace(errStr))
		return commitFailed
	}
	code, _, _ := runGit(root, 15*time.Second, append([]string{"diff", "--cached", "--quiet", "--"}, paths...)...)
	if code == 0 {
		fmt.Fprintln(errOut, "[OK] 没有需要提交的工作区变更。")
		return commitSkipped
	}
	code, _, commitErr := runGit(root, 15*time.Second, append([]string{"commit", "-m", commitMsg, "--"}, paths...)...)
	if code == 0 {
		fmt.Fprintf(errOut, "[OK] 已自动提交：%s\n", commitMsg)
		return commitDone
	}
	fmt.Fprintf(errOut, "[警告] 自动提交失败：%s\n", strings.TrimSpace(commitErr))
	return commitFailed
}

func printCommitCheckpoint(sessionNum int, journalName string, errOut io.Writer) {
	fmt.Fprintln(errOut, "")
	fmt.Fprintf(errOut, "[受阻] 检查点：会话 %d 已记录在 %s 与 index.md，但自动提交未完成。\n", sessionNum, journalName)
	fmt.Fprintln(errOut, "[受阻] 待处理记录保持原样，未回滚任何内容。")
	fmt.Fprintln(errOut, "[受阻] 修复上面的 git 错误后，重新运行完全相同的 add-session 命令：它会从提交步骤继续，不会追加第二条会话。")
}

// getStringField 是 rawObject.getString 的小包装（忽略“不存在”）。
func (o *rawObject) getStringField(field string) string {
	s, _ := o.getString(field)
	return s
}
