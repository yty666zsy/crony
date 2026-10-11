package models

import (
	"reflect"
	"testing"
)

// TestSerializeArgvRoundTrip 是阶段 2 "自然语言建任务"的关键不变式：
//
//	argv 数组 → Job.Command（落库字符串）→ SplitCmd() → **同一个 argv**
//
// 只要这条不变式成立，"模型给出的命令数组"在执行时就不会因为空白/引号被悄悄改写；
// 反例（直接 strings.Join）在参数含空格时会把一个参数切成两个，不报错、只跑错。
func TestSerializeArgvRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{"简单命令", []string{"/usr/bin/find", "/tmp", "-mtime", "+7", "-delete"}},
		{"单元素无参数", []string{"uptime"}},
		{"参数含空格", []string{"tar", "-czf", "/backup/my file.tar.gz", "/data"}},
		{"参数含双引号", []string{"sh", "-c", `echo "hello world"`}},
		{"参数含单引号", []string{"sh", "-c", `echo 'a b'`}},
		{"参数含反斜杠", []string{"grep", `\d+`, "/var/log/app.log"}},
		{"参数含中文与空格", []string{"echo", "备份 完成"}},
		{"参数含 tab", []string{"awk", "{print\t$1}"}},
		{"纯空白参数", []string{"echo", " "}},
		{"多空白参数", []string{"echo", "a  b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := SerializeArgv(tc.argv)
			job := &Job{Command: stored, Type: JobTypeCmd}
			job.SplitCmd()
			if !reflect.DeepEqual(job.Cmd, tc.argv) {
				t.Fatalf("argv 往返不一致\n原始: %#v\n落库: %q\n还原: %#v", tc.argv, stored, job.Cmd)
			}
			// 再把落库字符串喂给 Check()：它也会调用 SplitCmd，结果必须一致
			job2 := &Job{Name: "t", Command: stored, Type: JobTypeCmd}
			if err := job2.Check(); err != nil {
				t.Fatalf("Check 不应失败: %v", err)
			}
			if !reflect.DeepEqual(job2.Cmd, tc.argv) {
				t.Fatalf("Check 之后的 argv 不一致\n原始: %#v\n还原: %#v", tc.argv, job2.Cmd)
			}
		})
	}
}

// TestSerializeArgvKeepsSimpleCommandReadable 常见命令不应被加一堆引号：
// 落库字符串要人可读（运维排查时会直接看 job.command）。
func TestSerializeArgvKeepsSimpleCommandReadable(t *testing.T) {
	got := SerializeArgv([]string{"/usr/bin/find", "/tmp", "-mtime", "+7", "-delete"})
	want := "/usr/bin/find /tmp -mtime +7 -delete"
	if got != want {
		t.Fatalf("SerializeArgv = %q, want %q", got, want)
	}
}

// TestSerializeArgvCannotExpressEmptyArg 记录一个**已知且被显式拦截**的限制。
//
// Job.Command 是字符串、执行前由 SplitCmd（基于 utils.ParseCmdArguments）切分，
// 而该解析器不会产出空参数：`""` 会被丢弃。这不是本阶段引入的缺陷（阶段 1 已有），
// 但"自然语言建任务"会从模型拿到 argv 数组，因此必须在**写入前**拦住这种草案，
// 而不是写入后静默丢掉一个参数 —— 后者会让命令跑错且毫无提示。
//
// 拦截点在 agent.ValidateDraft（见 agent 包的 TestValidateDraftRejectsEmptyArg）。
func TestSerializeArgvCannotExpressEmptyArg(t *testing.T) {
	job := &Job{Command: SerializeArgv([]string{"cmd", ""}), Type: JobTypeCmd}
	job.SplitCmd()
	if len(job.Cmd) != 1 {
		t.Fatalf("预期空参数被丢弃（已知限制），实际 %#v", job.Cmd)
	}
}
