package tools

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// 本文件是**安全守卫**：只读 SQL、只读 shell 白名单、URL（防 SSRF）。
//
// 统一原则：**白名单 + 拒绝歧义**。
// 安全过滤最怕"我以为它能拦住"——所以每个守卫的失败都是"拒绝执行"，
// 而不是"尝试清洗后执行"（清洗会引入新的绕过面）。

// ---------------------------------------------------------------------------
// 只读 SQL 守卫
// ---------------------------------------------------------------------------

// sqlDenyKeywords 禁止出现的关键字（按词边界匹配，大小写不敏感）。
//
// 覆盖思路：写操作、DDL、权限、存储过程、文件读写、锁、事务、以及
// "拖死数据库"的函数（sleep/benchmark）。
var sqlDenyKeywords = []string{
	"insert", "update", "delete", "drop", "alter", "create", "truncate",
	"replace", "rename", "merge", "upsert",
	"grant", "revoke", "call", "execute", "prepare", "deallocate", "handler", "do",
	"load_file", "outfile", "dumpfile", "into",
	"set", "lock", "unlock", "begin", "commit", "rollback", "savepoint", "start", "transaction",
	"sleep", "benchmark", "get_lock", "release_lock", "shutdown", "kill", "flush",
	"purge", "install", "uninstall", "reset", "optimize", "repair", "analyze", "analyse",
	"procedure", "trigger", "event", "function", "view",
}

var sqlDenyPatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(sqlDenyKeywords))
	for _, kw := range sqlDenyKeywords {
		out = append(out, regexp.MustCompile(`\b`+regexp.QuoteMeta(kw)+`\b`))
	}
	return out
}()

// sqlLimitRe 匹配结尾的 `LIMIT n` 或 `LIMIT off, n`。
var sqlLimitRe = regexp.MustCompile(`(?i)\blimit\s+(\d+)(?:\s*,\s*(\d+))?\s*$`)

var sqlSpaceRe = regexp.MustCompile(`\s+`)

// GuardSQL 校验只读 SQL，返回规整后的查询。
//
// 规则（全部为"拒绝"语义，不做改写）：
//  1. 必须以 SELECT 开头（**不支持** `WITH ... SELECT`：CTE 里藏写操作难以可靠识别，
//     宁可不支持；需要 CTE 的场景请走阶段 3 的受控查询模板）；
//  2. 只允许单条语句（出现 `;` 直接拒绝）；
//  3. 拒绝注释（`--`、`/*`、`#`）——注释是隐藏关键字的经典手法；
//  4. 拒绝写/DDL/权限/事务/文件读写等关键字；
//  5. **必须显式带 LIMIT，且行数不超过 maxRows**。
//     不自动加 LIMIT：改写 SQL 可能改变语义（尤其是带 ORDER BY 的查询）。
func GuardSQL(query string, maxRows int) (string, error) {
	if maxRows <= 0 {
		maxRows = 100
	}
	q := strings.TrimSpace(query)
	if q == "" {
		return "", fmt.Errorf("%w: SQL 为空", ErrInvalidArgs)
	}
	if strings.Contains(q, ";") {
		return "", fmt.Errorf("%w: 只允许单条语句（出现分号）", ErrReadonlyViolation)
	}
	if strings.Contains(q, "--") || strings.Contains(q, "/*") || strings.Contains(q, "#") {
		return "", fmt.Errorf("%w: 不允许 SQL 注释", ErrReadonlyViolation)
	}
	normalized := sqlSpaceRe.ReplaceAllString(strings.ToLower(q), " ")
	if !strings.HasPrefix(normalized, "select ") && normalized != "select" && !strings.HasPrefix(normalized, "select\t") {
		if !strings.HasPrefix(normalized, "select") {
			return "", fmt.Errorf("%w: 只允许 SELECT 查询（当前以 %q 开头）", ErrReadonlyViolation, firstWord(normalized))
		}
	}
	for i, re := range sqlDenyPatterns {
		if re.MatchString(normalized) {
			return "", fmt.Errorf("%w: 出现禁止关键字 %q", ErrReadonlyViolation, sqlDenyKeywords[i])
		}
	}
	m := sqlLimitRe.FindStringSubmatch(normalized)
	if m == nil {
		return "", fmt.Errorf("%w: 必须显式带 LIMIT（且不超过 %d 行）", ErrReadonlyViolation, maxRows)
	}
	rowsToken := m[1]
	if m[2] != "" {
		rowsToken = m[2]
	}
	rows, err := strconv.Atoi(rowsToken)
	if err != nil {
		return "", fmt.Errorf("%w: LIMIT 不是合法数字 %q", ErrInvalidArgs, rowsToken)
	}
	if rows <= 0 {
		return "", fmt.Errorf("%w: LIMIT 必须为正数", ErrReadonlyViolation)
	}
	if rows > maxRows {
		return "", fmt.Errorf("%w: LIMIT %d 超过上限 %d", ErrReadonlyViolation, rows, maxRows)
	}
	return q, nil
}

// firstWord 取首个词（用于错误提示）。
func firstWord(s string) string {
	if idx := strings.IndexAny(s, " \t\n"); idx > 0 {
		return s[:idx]
	}
	return s
}

// ---------------------------------------------------------------------------
// 只读 shell 白名单
// ---------------------------------------------------------------------------

// ErrReadonlyViolation 违反只读约束。
var ErrReadonlyViolation = fmt.Errorf("tools: 违反只读约束")

// shellMetaChars 这些字符一律拒绝。
//
// 即使我们**不经过 shell**（exec.Command 直接执行），也要拒绝：
// 它们在任何命令里都没有正当用途，却能让人误以为"过滤了分号就安全"。
const shellMetaChars = ";|&$`><(){}[]!*?~^\"'\n\r\t\\"

// flagSpec 选项声明。
type flagSpec struct {
	takesValue bool
	valueRule  func(string) error
}

func numericValue(name string, min, max int) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%w: %s 的值 %q 不是数字", ErrInvalidArgs, name, v)
		}
		if n < min || n > max {
			return fmt.Errorf("%w: %s 的值 %d 超出范围 [%d, %d]", ErrInvalidArgs, name, n, min, max)
		}
		return nil
	}
}

func enumValue(name string, allowed ...string) func(string) error {
	return func(v string) error {
		for _, a := range allowed {
			if v == a {
				return nil
			}
		}
		return fmt.Errorf("%w: %s 的值 %q 不在允许集合 %v 内", ErrInvalidArgs, name, v, allowed)
	}
}

// shellRules 每个允许命令的规则。
//
// 阶段 2 只放四个命令（方案原文的白名单）。**刻意不放 cat/grep/find/awk**：
// 它们能读任意文件（`cat /etc/shadow`）或执行任意程序（`find -exec`），
// 一旦放开，"只读"就等于"任意读 + 任意执行"。
var shellRules = map[string]struct {
	maxArgs         int
	minPositional   int
	shortFlags      map[string]flagSpec
	longFlags       map[string]flagSpec
	positionalCheck func(string, Policy) error
	// anyFlag 为 true 时，形如 `-ef` 的 token 也按 positionalCheck 校验
	// （用于 ps 这类"选项风格不统一"的命令）。
	anyFlag     bool
	description string
}{
	"df": {
		maxArgs:       8,
		minPositional: 0,
		shortFlags: map[string]flagSpec{
			"h": {}, "k": {}, "T": {}, "P": {}, "i": {}, "a": {}, "l": {}, "H": {}, "x": {takesValue: true},
			"t": {takesValue: true}, "B": {takesValue: true, valueRule: enumValue("-B", "1K", "1M", "1G", "1k", "1m", "1g", "K", "M", "G")},
		},
		longFlags: map[string]flagSpec{
			"help": {}, "human-readable": {}, "print-type": {}, "portability": {}, "inodes": {}, "all": {}, "local": {},
		},
		positionalCheck: func(arg string, _ Policy) error {
			if !strings.HasPrefix(arg, "/") {
				return fmt.Errorf("%w: df 的路径参数必须是绝对路径: %q", ErrInvalidArgs, arg)
			}
			return guardCleanPath(arg)
		},
		description: "查看文件系统使用率",
	},
	"ps": {
		maxArgs:       8,
		minPositional: 0,
		// ps 的选项风格混乱（BSD/GNU 两套），这里只做"字符白名单"约束：
		// 允许 字母/数字/逗号/等号/点/斜杠/加号/减号，且必须不含 shell 元字符。
		//
		// anyFlag=true：把 `-ef`/`-eo`/`-aux` 这类选项也交给 positionalCheck 校验
		// （它们本来就不匹配白名单里的任何具名选项，但仍属于"安全 token"）。
		anyFlag: true,
		positionalCheck: func(arg string, _ Policy) error {
			if regexp.MustCompile(`^[a-zA-Z0-9,=._+/-]+$`).MatchString(arg) {
				return nil
			}
			return fmt.Errorf("%w: ps 参数 %q 含不允许的字符", ErrInvalidArgs, arg)
		},
		description: "查看进程列表",
	},
	"tail": {
		maxArgs:       6,
		minPositional: 1,
		shortFlags: map[string]flagSpec{
			"n": {takesValue: true, valueRule: numericValue("-n", 1, 10000)},
			"c": {takesValue: true, valueRule: numericValue("-c", 1, 1<<20)},
			"q": {}, "v": {},
		},
		longFlags: map[string]flagSpec{
			"lines": {takesValue: true, valueRule: numericValue("--lines", 1, 10000)},
			"bytes": {takesValue: true, valueRule: numericValue("--bytes", 1, 1<<20)},
			"quiet": {}, "verbose": {},
		},
		positionalCheck: func(arg string, policy Policy) error {
			// 解析软链后再校验：`/var/log/x.log -> /etc/shadow` 会让目录白名单形同虚设
			_, err := ResolveReadPath(arg, policy)
			return err
		},
		description: "查看日志文件尾部（只允许读取白名单目录）",
	},
	"journalctl": {
		maxArgs:       10,
		minPositional: 0,
		shortFlags: map[string]flagSpec{
			"u": {takesValue: true}, "n": {takesValue: true, valueRule: numericValue("-n", 1, 10000)},
			"p": {takesValue: true, valueRule: enumValue("-p", "emerg", "alert", "crit", "err", "warning", "notice", "info", "debug", "0", "1", "2", "3", "4", "5", "6", "7")},
			"o": {takesValue: true, valueRule: enumValue("-o", "short", "cat", "json", "json-pretty", "verbose")},
			"S": {takesValue: true}, "U": {takesValue: true}, "b": {}, "k": {}, "q": {},
		},
		longFlags: map[string]flagSpec{
			"unit": {takesValue: true}, "lines": {takesValue: true, valueRule: numericValue("--lines", 1, 10000)},
			"since": {takesValue: true}, "until": {takesValue: true}, "priority": {takesValue: true},
			"output": {takesValue: true}, "no-pager": {}, "dmesg": {}, "quiet": {}, "utc": {},
			"identifier": {takesValue: true}, "boot": {},
		},
		positionalCheck: func(arg string, _ Policy) error {
			// journalctl 的匹配模式（如 _SYSTEMD_UNIT=sshd.service）
			if regexp.MustCompile(`^[A-Za-z0-9_.=@:/-]+$`).MatchString(arg) {
				return nil
			}
			return fmt.Errorf("%w: journalctl 参数 %q 含不允许的字符", ErrInvalidArgs, arg)
		},
		description: "查询 systemd 日志",
	},
}

// AllowedShellCommands 返回允许的命令名（用于错误提示与文档）。
func AllowedShellCommands() []string {
	out := make([]string, 0, len(shellRules))
	for name := range shellRules {
		out = append(out, name)
	}
	sortStrings(out)
	return out
}

// GuardShellCommand 校验"命令 + 参数"是否落在只读白名单内。
//
// 校验完**仍然不用 shell 执行**（见 ShellTool）。这里拒绝元字符是为了
// "即使将来有人改成 shell 执行也不会立刻变成 RCE"。
func GuardShellCommand(cmd string, args []string, policy Policy) error {
	rule, ok := shellRules[cmd]
	if !ok {
		return fmt.Errorf("%w: 命令 %q 不在只读白名单（允许: %s）", ErrToolDenied, cmd, strings.Join(AllowedShellCommands(), ", "))
	}
	if len(args) > rule.maxArgs {
		return fmt.Errorf("%w: %s 参数过多（%d > %d）", ErrInvalidArgs, cmd, len(args), rule.maxArgs)
	}
	if rule.maxArgs == 0 && len(args) > 0 {
		return fmt.Errorf("%w: %s 不接受参数", ErrInvalidArgs, cmd)
	}

	positional := 0
	pendingValue := func(string) error { return nil }
	expectValue := false
	for _, arg := range args {
		if arg == "" {
			continue
		}
		if strings.ContainsAny(arg, shellMetaChars) {
			return fmt.Errorf("%w: 参数 %q 含 shell 元字符", ErrInvalidArgs, arg)
		}
		if expectValue {
			if err := pendingValue(arg); err != nil {
				return err
			}
			expectValue = false
			continue
		}
		if strings.HasPrefix(arg, "--") {
			if rule.anyFlag && rule.positionalCheck != nil {
				if err := rule.positionalCheck(arg, policy); err != nil {
					return err
				}
				continue
			}
			if rule.longFlags == nil {
				return fmt.Errorf("%w: %s 不支持长选项 %q", ErrInvalidArgs, cmd, arg)
			}
			name := strings.TrimPrefix(arg, "--")
			value := ""
			hasInline := false
			if idx := strings.Index(name, "="); idx >= 0 {
				value, hasInline = name[idx+1:], true
				name = name[:idx]
			}
			spec, ok := rule.longFlags[name]
			if !ok {
				return fmt.Errorf("%w: %s 不支持选项 %q", ErrInvalidArgs, cmd, arg)
			}
			if spec.takesValue || hasInline {
				if !hasInline {
					expectValue = true
					pendingValue = spec.valueRule
					if pendingValue == nil {
						pendingValue = func(string) error { return nil }
					}
					continue
				}
				if spec.valueRule != nil {
					if err := spec.valueRule(value); err != nil {
						return err
					}
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && len(arg) > 1 {
			if rule.anyFlag && rule.positionalCheck != nil {
				if err := rule.positionalCheck(arg, policy); err != nil {
					return err
				}
				continue
			}
			if rule.shortFlags == nil {
				return fmt.Errorf("%w: %s 不支持短选项 %q", ErrInvalidArgs, cmd, arg)
			}
			// 纯数字短选项：tail -50 形式
			if regexp.MustCompile(`^-\d+$`).MatchString(arg) && cmd == "tail" {
				n, err := strconv.Atoi(strings.TrimPrefix(arg, "-"))
				if err != nil || n < 1 || n > 10000 {
					return fmt.Errorf("%w: %s 的行数 %q 超范围", ErrInvalidArgs, cmd, arg)
				}
				continue
			}
			body := strings.TrimPrefix(arg, "-")
			if len(body) == 1 {
				spec, ok := rule.shortFlags[body]
				if !ok {
					return fmt.Errorf("%w: %s 不支持选项 %q", ErrInvalidArgs, cmd, arg)
				}
				if spec.takesValue {
					expectValue = true
					pendingValue = spec.valueRule
					if pendingValue == nil {
						pendingValue = func(string) error { return nil }
					}
				}
				continue
			}
			// 组合短选项：要求全部存在且都不需要取值（避免 -n5 这种歧义写法）
			for _, ch := range body {
				spec, ok := rule.shortFlags[string(ch)]
				if !ok {
					return fmt.Errorf("%w: %s 不支持选项 %q", ErrInvalidArgs, cmd, arg)
				}
				if spec.takesValue {
					return fmt.Errorf("%w: 选项 %q 必须单独给出取值（不支持 -n5 写法）", ErrInvalidArgs, arg)
				}
			}
			continue
		}
		positional++
		if rule.positionalCheck != nil {
			if err := rule.positionalCheck(arg, policy); err != nil {
				return err
			}
		}
	}
	if expectValue {
		return fmt.Errorf("%w: %s 的选项缺少取值", ErrInvalidArgs, cmd)
	}
	if positional < rule.minPositional {
		return fmt.Errorf("%w: %s 至少需要 %d 个路径参数", ErrInvalidArgs, cmd, rule.minPositional)
	}
	return nil
}

// guardCleanPath 拒绝含 `..` 的路径（防止用相对穿越绕过目录白名单）。
func guardCleanPath(p string) error {
	if strings.Contains(p, "..") {
		return fmt.Errorf("%w: 路径 %q 含 ..（疑似目录穿越）", ErrReadonlyViolation, p)
	}
	return nil
}

// guardReadPath 校验可读路径落在白名单前缀内。
func guardReadPath(p string, policy Policy) error {
	if err := guardCleanPath(p); err != nil {
		return err
	}
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: 路径 %q 必须是绝对路径", ErrInvalidArgs, p)
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	prefixes := policy.Normalize().ShellReadPaths
	for _, prefix := range prefixes {
		prefix = filepath.ToSlash(prefix)
		if prefix == "" {
			continue
		}
		if strings.HasSuffix(prefix, "/") {
			if strings.HasPrefix(cleaned, prefix) {
				return nil
			}
			continue
		}
		if cleaned == prefix || strings.HasPrefix(cleaned, prefix+"/") {
			return nil
		}
	}
	return fmt.Errorf("%w: 路径 %q 不在允许读取的目录 %v 内", ErrToolDenied, cleaned, prefixes)
}

// ResolveReadPath 解析符号链接后再做一次白名单校验。
//
// 为什么必须做：`/var/log/app.log -> /etc/shadow` 这类软链会让"目录白名单"
// 形同虚设。文件不存在时按原路径校验（tail 自己会报错）。
func ResolveReadPath(p string, policy Policy) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		if err := guardReadPath(p, policy); err != nil {
			return "", err
		}
		return p, nil
	}
	if err := guardReadPath(resolved, policy); err != nil {
		return "", err
	}
	return resolved, nil
}

// ---------------------------------------------------------------------------
// URL 守卫（防 SSRF）
// ---------------------------------------------------------------------------

// blockedMetadataHosts 云元数据端点：**硬拒绝**，即使被写进 AllowHosts。
//
// 这是 SSRF 里危害最大的一类（拿到实例凭据 = 拿到云上权限），
// 没有"业务上确实需要"的正当理由，所以不给配置开关。
var blockedMetadataHosts = []string{
	"metadata.google.internal",
	"metadata.goog",
	"100.100.100.200", // 阿里云
	"fd00:ec2::254",   // AWS IPv6
}

// GuardURL 校验 http_get 的目标。
func GuardURL(raw string, allowHosts []string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("%w: url 为空", ErrInvalidArgs)
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: url 解析失败: %v", ErrInvalidArgs, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: 只允许 http/https（当前 %q）", ErrReadonlyViolation, u.Scheme)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%w: url 不允许携带用户名/密码", ErrReadonlyViolation)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("%w: url 缺少主机名", ErrInvalidArgs)
	}
	for _, blocked := range blockedMetadataHosts {
		if host == blocked {
			return nil, fmt.Errorf("%w: %s 是云元数据端点，永久禁止访问", ErrToolDenied, host)
		}
	}
	if isLinkLocal(host) {
		return nil, fmt.Errorf("%w: %s 属于链路本地地址（169.254.0.0/16，元数据高危区），永久禁止访问", ErrToolDenied, host)
	}
	if len(allowHosts) == 0 {
		return nil, fmt.Errorf("%w: 未配置 AllowHosts，默认禁止一切外部请求（这是刻意的默认值）", ErrToolDenied)
	}
	if !hostAllowed(host, allowHosts) {
		return nil, fmt.Errorf("%w: 主机 %s 不在 AllowHosts %v 内", ErrToolDenied, host, allowHosts)
	}
	return u, nil
}

// isLinkLocal 判断是否落在 169.254.0.0/16。
func isLinkLocal(host string) bool {
	if strings.HasPrefix(host, "169.254.") {
		return true
	}
	return false
}

// hostAllowed 主机名白名单匹配，支持 `*.example.com`。
//
// 注意：`*.example.com` **不**匹配 `example.com` 本身（避免"以为收窄了其实放开了根域"）。
func hostAllowed(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	for _, pattern := range patterns {
		p := strings.ToLower(strings.TrimSpace(pattern))
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "*.") {
			suffix := strings.TrimPrefix(p, "*")
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
			continue
		}
		if host == p {
			return true
		}
	}
	return false
}

// sortStrings 本地排序，避免为一个函数引入额外依赖。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
