package task

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/linrong007/Sunshine/internal/config"
)

// ---------------------------------------------------------------------------
// 配置补充读取（session_auto_commit / session_commit_message / hooks）
//
// 由 internal/config 的完整解析器（yaml.v3）提供：task 命令只读
// 这三个键。配置文件缺失或损坏时一律返回默认值，任务命令不因
// 配置问题失败（与 Trellis 脚本的默认策略一致）。
// ---------------------------------------------------------------------------

// ConfigExtras 是 config.yaml 里 task 命令需要的补充字段。
type ConfigExtras struct {
	// SessionAutoCommit 默认 true；显式 false 才关闭自动提交。
	SessionAutoCommit bool
	// SessionCommitMessage 默认 "chore: record journal"。
	SessionCommitMessage string
	// Hooks 是事件名到 shell 命令列表的映射。
	Hooks map[string][]string
}

// LoadConfigExtras 读 .sunshine/config.yaml 里的补充字段。
func LoadConfigExtras(root string) ConfigExtras {
	def := ConfigExtras{
		SessionAutoCommit:    true,
		SessionCommitMessage: "chore: record journal",
		Hooks:                map[string][]string{},
	}
	cfg, err := config.LoadProject(root)
	if err != nil {
		return def
	}
	msg := cfg.SessionCommitMessage
	if strings.TrimSpace(msg) == "" {
		msg = def.SessionCommitMessage
	}
	return ConfigExtras{
		SessionAutoCommit:    cfg.AutoCommit(),
		SessionCommitMessage: msg,
		Hooks: map[string][]string{
			"after_create":  cfg.Hooks.AfterCreate,
			"after_start":   cfg.Hooks.AfterStart,
			"after_finish":  cfg.Hooks.AfterFinish,
			"after_archive": cfg.Hooks.AfterArchive,
		},
	}
}

// ---------------------------------------------------------------------------
// 生命周期 hooks（config.yaml 的 hooks.<event>，shell 命令，60 秒超时，
// 失败只警告不阻断，对应 task_utils.run_task_hooks）。
// ---------------------------------------------------------------------------

const (
	hookTimeoutSeconds = 60
	hookOutputLimit    = 2000
)

// RunTaskHooks 执行某事件的 hooks；无配置时直接返回。
func RunTaskHooks(event, taskJSONPath, root string, errOut *os.File) {
	extras := LoadConfigExtras(root)
	commands := extras.Hooks[event]
	if len(commands) == 0 {
		return
	}
	for _, cmdStr := range commands {
		runOneHook(event, cmdStr, taskJSONPath, root, errOut)
	}
}

func runOneHook(event, cmdStr, taskJSONPath, root string, errOut *os.File) {
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeoutSeconds*time.Second)
	defer cancel()
	cmd := newHookCmd(ctx, cmdStr)
	cmd.Dir = root
	env := os.Environ()
	env = append(env, "TASK_JSON_PATH="+taskJSONPath)
	cmd.Env = env
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if ctx.Err() != nil {
		if errOut != nil {
			fmt.Fprintf(errOut, "[警告] Hook 超时（%s），%d 秒未返回：%s\n  工作目录：%s\n", event, hookTimeoutSeconds, cmdStr, root)
			printHookStream("stdout", outBuf.String(), errOut)
			printHookStream("stderr", errBuf.String(), errOut)
		}
		return
	}
	if err != nil {
		if errOut != nil {
			fmt.Fprintf(errOut, "[警告] Hook 失败（%s）：%s，命令：%s\n  工作目录：%s\n", event, hookExitCode(err), cmdStr, root)
			printHookStream("stdout", outBuf.String(), errOut)
			printHookStream("stderr", errBuf.String(), errOut)
		}
	}
}

func hookExitCode(err error) string {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return fmt.Sprintf("退出码 %d", exitErr.ExitCode())
	}
	return err.Error()
}

func printHookStream(name, text string, errOut *os.File) {
	if errOut == nil {
		return
	}
	if len(text) > hookOutputLimit {
		text = text[:hookOutputLimit] + "…（已截断）"
	}
	fmt.Fprintf(errOut, "  %s：%s\n", name, text)
}

// ---------------------------------------------------------------------------
// 安全 git 提交（对应 safe_commit.py：只 stage 明确的产品路径，绝不
// git add -f；.gitignore 拦截时警告并跳过）。
// ---------------------------------------------------------------------------

// ignoredSubpaths 是建议用户在 .gitignore 里单独忽略的子路径。
var ignoredSubpaths = []string{
	".sunshine/.backup-*",
	".sunshine/worktrees/",
	".sunshine/.template-hashes.json",
	".sunshine/.runtime/",
	".sunshine/.cache/",
}

// safeTrellisPathsToAdd 返回 add-session 自动提交要 stage 的路径：
// 开发者 journal + index.md，以及当前任务目录（仅当前任务）。
func safeTrellisPathsToAdd(root, taskName string) []string {
	var paths []string
	dev := Developer(root)
	if dev != "" {
		ws := filepath.Join(root, DirSunshine, "workspace", dev)
		if st, err := os.Stat(ws); err == nil && st.IsDir() {
			entries, _ := os.ReadDir(ws)
			for _, e := range entries {
				if !e.IsDir() && strings.HasPrefix(e.Name(), "journal-") && strings.HasSuffix(e.Name(), ".md") {
					paths = append(paths, DirSunshine+"/workspace/"+dev+"/"+e.Name())
				}
			}
			if _, err := os.Stat(filepath.Join(ws, "index.md")); err == nil {
				paths = append(paths, DirSunshine+"/workspace/"+dev+"/index.md")
			}
		}
	}
	if taskName != "" {
		tasksDir := filepath.Join(root, DirSunshine, DirTasks)
		active := filepath.Join(tasksDir, taskName)
		if st, err := os.Stat(active); err == nil && st.IsDir() {
			paths = append(paths, DirSunshine+"/"+DirTasks+"/"+taskName)
		}
		archived := filepath.Join(tasksDir, DirArchive, taskName)
		if st, err := os.Stat(archived); err == nil && st.IsDir() {
			paths = append(paths, DirSunshine+"/"+DirTasks+"/"+DirArchive+"/"+taskName)
		}
	}
	// 项目根 .gitattributes：add-session 调 EnsureGitattributes 追加了 journal
	// merge=union 规则（见下），没有它这条规则永远进不了提交。
	// 仅当文件存在且含该规则时才 stage——用户自己的 .gitattributes 不动。
	if data, err := os.ReadFile(filepath.Join(root, ".gitattributes")); err == nil && journalMergeUnionRe.Match(data) {
		paths = append(paths, ".gitattributes")
	}
	return paths
}

// safeArchivePathsToAdd 返回 archive 自动提交要 stage 的路径：
// archive 子树 + 被改 parent 的子女任务目录。
func safeArchivePathsToAdd(root string, modifiedChildren []string) []string {
	var paths []string
	archiveDir := filepath.Join(root, DirSunshine, DirTasks, DirArchive)
	if st, err := os.Stat(archiveDir); err == nil && st.IsDir() {
		paths = append(paths, DirSunshine+"/"+DirTasks+"/"+DirArchive)
	}
	for _, child := range modifiedChildren {
		paths = append(paths, DirSunshine+"/"+DirTasks+"/"+child)
	}
	return paths
}

// safeGitAdd 对指定路径 git add（不 -f）。返回（成功，stderr）。
func safeGitAdd(root string, paths []string, retryOnIndexLock bool) (bool, string) {
	if len(paths) == 0 {
		return true, ""
	}
	args := append([]string{"add", "--"}, paths...)
	var code int
	var errStr string
	if retryOnIndexLock {
		code, _, errStr = runGitRetryIndexLock(root, args...)
	} else {
		code, _, errStr = runGit(root, 15*time.Second, args...)
	}
	return code == 0, errStr
}

// printGitignoreWarning 打印 .gitignore 拦截警告（含“不要 git add -f”的
// 反例，对应 safe_commit.print_gitignore_warning）。
func printGitignoreWarning(paths []string, errOut *os.File) {
	if errOut == nil {
		return
	}
	fmt.Fprintln(errOut, "[警告] git add 失败：.sunshine/ 路径被 .gitignore 忽略。")
	fmt.Fprintln(errOut, "[警告] 跳过自动提交。journal/任务文件已写入磁盘，git 未动。")
	fmt.Fprintln(errOut, "[警告]")
	fmt.Fprintln(errOut, "[警告] Sunshine 管理这些具体路径，应纳入版本控制：")
	if len(paths) > 0 {
		for _, p := range paths {
			fmt.Fprintf(errOut, "[警告]   %s\n", p)
		}
	} else {
		fmt.Fprintln(errOut, "[警告]   .sunshine/workspace/<开发者>/{journal-*.md,index.md}")
		fmt.Fprintln(errOut, "[警告]   .sunshine/tasks/<任务目录>/")
		fmt.Fprintln(errOut, "[警告]   .sunshine/tasks/archive/")
	}
	fmt.Fprintln(errOut, "[警告]")
	fmt.Fprintln(errOut, "[警告] 建议：把 .gitignore 里的 `.sunshine/` 改成只忽略以下子路径：")
	for _, sub := range ignoredSubpaths {
		fmt.Fprintf(errOut, "[警告]   %s\n", sub)
	}
	fmt.Fprintln(errOut, "[警告]")
	fmt.Fprintln(errOut, "[警告] 或在 .sunshine/config.yaml 里设置 session_auto_commit: false，")
	fmt.Fprintln(errOut, "[警告] 让脚本完全跳过 git，由你手动 git status / git add / git commit。")
	fmt.Fprintln(errOut, "[警告]")
	fmt.Fprintln(errOut, "[警告] 不要用 `git add -f .sunshine/`——它会把备份、worktree 与运行时缓存一起提交。")
}

func printIndexLockWarning(action, taskName, root string, paths []string, errOut *os.File) {
	if errOut == nil {
		return
	}
	lock := ".git/index.lock"
	if code, out, _ := runGit(root, 15*time.Second, "rev-parse", "--git-path", "index.lock"); code == 0 && strings.TrimSpace(out) != "" {
		lock = strings.TrimSpace(out)
	}
	fmt.Fprintf(errOut, "[警告] %s 在多次尝试后放弃：另一进程正持有 %s\n", action, lock)
	fmt.Fprintln(errOut, "[警告] 任务已移入 archive/，只有提交待办。")
	fmt.Fprintln(errOut, "[警告] 请关闭持有锁的进程（IDE git 集成、git 状态守护进程、另一会话），")
	fmt.Fprintf(errOut, "[警告] 或确认锁已过期后删除 %s，再手动提交：git add -A -- %s && git commit -m \"chore(task): archive %s\"\n", lock, strings.Join(paths, " "), taskName)
}

// gitIdentityMissing 检查 git 身份是否缺失，返回缺失的键名
// （user.name / user.email）。`git config <键>` 退出码非 0 或值为空都算缺失。
func gitIdentityMissing(root string) []string {
	var missing []string
	for _, key := range []string{"user.name", "user.email"} {
		code, out, _ := runGit(root, 15*time.Second, "config", key)
		if code != 0 || strings.TrimSpace(out) == "" {
			missing = append(missing, key)
		}
	}
	return missing
}

// printGitIdentityWarning 打印 git 身份缺失警告（风格同 printGitignoreWarning）：
// 说明缺了哪项、文件已写盘 git 未动、给修复方法。调用方跳过自动提交即可，
// 不要让整个归档/add-session 操作失败。
func printGitIdentityWarning(missing []string, errOut io.Writer) {
	if errOut == nil {
		return
	}
	fmt.Fprintf(errOut, "[警告] git 身份未设置（缺失：%s），自动提交已跳过。\n", strings.Join(missing, "、"))
	fmt.Fprintln(errOut, "[警告] 任务/会话文件已写入磁盘，git 未动。")
	fmt.Fprintln(errOut, "[警告]")
	fmt.Fprintln(errOut, "[警告] 修复方法（二选一）：")
	fmt.Fprintln(errOut, "[警告]   1. 设置 git 身份：git config user.name \"名字\"；git config user.email \"邮箱\"")
	fmt.Fprintln(errOut, "[警告]   2. 或在 .sunshine/config.yaml 里设置 session_auto_commit: false，让脚本完全跳过 git")
}

// autoCommitArchive 是 archive 的自动提交（对应 _auto_commit_archive）。
// 返回是否“可以继续按成功处理”（提交失败但源任务未被追踪时也返回 true，
// 因为磁盘移动已完成且无历史可写）。
func autoCommitArchive(root, taskName string, modifiedChildren []string, errOut *os.File) bool {
	extras := LoadConfigExtras(root)
	if !extras.SessionAutoCommit {
		fmt.Fprintln(errOut, "[OK] session_auto_commit: false——跳过 git 提交。")
		return true
	}
	if missing := gitIdentityMissing(root); len(missing) > 0 {
		printGitIdentityWarning(missing, errOut)
		return true
	}
	sourceRel := DirSunshine + "/" + DirTasks + "/" + taskName
	code, trackedOut, _ := runGit(root, 15*time.Second, "ls-files", "--", sourceRel)
	sourceWasTracked := code == 0 && strings.TrimSpace(trackedOut) != ""
	paths := safeArchivePathsToAdd(root, modifiedChildren)
	if len(paths) == 0 {
		fmt.Fprintln(errOut, "[OK] 没有需要提交的任务变更。")
		return true
	}
	ok, errStr := safeGitAdd(root, paths, true)
	if !ok {
		if strings.Contains(strings.ToLower(errStr), "ignored by") {
			printGitignoreWarning(paths, errOut)
		} else if strings.Contains(strings.ToLower(errStr), "index.lock") {
			printIndexLockWarning("git add", taskName, root, append(append([]string{}, paths...), sourceRel), errOut)
		} else {
			fmt.Fprintf(errOut, "[警告] git add 失败：%s\n", strings.TrimSpace(errStr))
		}
		return !sourceWasTracked
	}
	// git add 不 stage 已移走的源目录删除，显式 git rm --cached 补上。
	code, _, errStr = runGitRetryIndexLock(root, "rm", "-r", "--cached", "--ignore-unmatch", "--", sourceRel)
	if code != 0 && strings.Contains(strings.ToLower(errStr), "index.lock") {
		printIndexLockWarning("git rm --cached", taskName, root, append(append([]string{}, paths...), sourceRel), errOut)
		return !sourceWasTracked
	}
	commitPaths := append(append([]string{}, paths...), sourceRel)
	if !sourceWasTracked {
		commitPaths = paths
	}
	code, _, _ = runGit(root, 15*time.Second, append([]string{"diff", "--cached", "--quiet", "--"}, commitPaths...)...)
	if code == 0 {
		fmt.Fprintln(errOut, "[OK] 没有需要提交的任务变更。")
		return true
	}
	commitMsg := "chore(task): archive " + taskName
	code, _, errStr = runGitRetryIndexLock(root, append([]string{"commit", "-m", commitMsg, "--"}, commitPaths...)...)
	if code == 0 {
		fmt.Fprintf(errOut, "[OK] 已自动提交：%s\n", commitMsg)
		return true
	}
	if strings.Contains(strings.ToLower(errStr), "index.lock") {
		printIndexLockWarning("git commit", taskName, root, append(append([]string{}, paths...), sourceRel), errOut)
		return !sourceWasTracked
	}
	fmt.Fprintf(errOut, "[警告] 自动提交失败：%s\n", strings.TrimSpace(errStr))
	return !sourceWasTracked
}

// ---------------------------------------------------------------------------
// .gitattributes：项目根追加 journal merge=union 规则（已存在不重复）。
// Trellis 在 init 时做这一步；阶段 1 的 scaffold 未做，add-session
// （journal 的写入方）在这里补上。
// ---------------------------------------------------------------------------

var journalMergeUnionRe = regexp.MustCompile(`journal-\*\.md\s+merge=union`)

// GitattributesRule 是要追加的规则文本（Sunshine 版，对应 Trellis 的
// templates/trellis/gitattributes.txt）。
const GitattributesRule = `# Sunshine：各会话只向开发者 journal 追加新块，不会真正冲突，用 merge=union 让并行会话/worktree 的 journal 合并干净。
#
# 不要给 workspace/*/index.md 加规则——它每次会话都会整段重算，真正的冲突按“任选一边”解决即可（任务状态在 task.json，不在 index.md）。
.sunshine/workspace/*/journal-*.md merge=union
`

// EnsureGitattributes 确保项目根 .gitattributes 含 journal merge=union
// 规则；已存在时不重复追加。
func EnsureGitattributes(root string) error {
	target := filepath.Join(root, ".gitattributes")
	existing, err := os.ReadFile(target)
	if err == nil {
		if journalMergeUnionRe.Match(existing) {
			return nil
		}
		sep := "\n"
		if !bytes.HasSuffix(existing, []byte("\n")) {
			sep = "\n\n"
		}
		return writeFileAtomic(target, append(append([]byte{}, existing...), append([]byte(sep), []byte(GitattributesRule)...)...), 0o644)
	}
	if !os.IsNotExist(err) {
		return err
	}
	return writeFileAtomic(target, []byte(GitattributesRule), 0o644)
}
