package service

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/utils"
)

// nodeIDFileName 节点身份文件。
const nodeIDFileName = "node.uuid"

// nodeIDPattern 只接受标准 UUID 形式（36 字符、连字符分隔）。
//
// 为什么要校验：这个文件是**节点身份**，一旦内容是空白/被截断/被别的程序写脏，
// 节点就会以一个新身份出现，历史探测请求、`job_run.node_uuid`、节点水位
// 全部对不上。宁可重新生成并在日志里告警，也不要带着脏身份启动。
var nodeIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// nodeIdentityDir 身份文件目录：优先用日志目录（已配置、可写、运维熟悉），
// 否则用进程当前目录（配置文件本来就是相对 CWD 找的，说明 CWD 可用）。
func nodeIdentityDir() string {
	cfg := config.GetConfigModels()
	if cfg != nil && strings.TrimSpace(cfg.Log.Director) != "" {
		return cfg.Log.Director
	}
	return "."
}

// LoadOrCreateNodeUUID 读取或创建本节点的**持久**身份。
//
// 为什么必须有这个函数（原实现的缺陷）：
// 上游 crony 每次启动都调 `utils.UUID()` 生成一个**随机** UUID 当节点身份。
// 于是节点一重启就"换了一个人"：`job_run.node_uuid` 变成无法归属的历史、
// `node_metric` 的水位断了线，而 admin→node 的只读探测请求会发给一个**永不存在的
// 节点 UUID**（探测通道直接不可用）。节点身份必须是稳定的，这是所有
// "按节点归集"能力的前置条件。
//
// dir 用于存放身份文件；为空时用进程当前目录。返回的第二个值是**告警文本**
// （非空表示身份没能持久化，本次仍可用但下次启动会变——必须写进日志，不能静默）。
func LoadOrCreateNodeUUID(dir string) (uuid string, warning string) {
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	path := filepath.Join(dir, nodeIDFileName)

	if raw, err := os.ReadFile(path); err == nil {
		got := strings.TrimSpace(string(raw))
		if nodeIDPattern.MatchString(got) {
			return got, ""
		}
		warning = fmt.Sprintf("节点身份文件 %s 内容不合法（%q），已重新生成", path, clipForLog(got, 64))
	} else if !os.IsNotExist(err) {
		warning = fmt.Sprintf("节点身份文件 %s 读取失败：%s；已重新生成", path, err.Error())
	}

	fresh, err := utils.UUID()
	if err != nil {
		// 连 UUID 都生成不出来：退回主机名（不稳定，但比空身份好，且必然告警）
		host, _ := os.Hostname()
		return host, fmt.Sprintf("生成 UUID 失败：%s；已退回主机名作为节点身份（不稳定）", err.Error())
	}
	if err := writeFileAtomic(path, fresh); err != nil {
		return fresh, fmt.Sprintf("节点身份无法持久化到 %s：%s；本次启动身份为 %s，重启后会变化", path, err.Error(), fresh)
	}
	return fresh, warning
}

// writeFileAtomic 先写临时文件再改名，避免"写了一半"留下脏身份文件。
func writeFileAtomic(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content+"\n"), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func clipForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
