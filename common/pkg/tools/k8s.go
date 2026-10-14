package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/pkg/jsonschema"
	"github.com/tmnhs/crony/common/pkg/llm"
)

// 本文件是阶段 K2 的**只读集群工具**：把 Kubernetes 纳入 Agent 的证据面。
//
// 为什么是"固定操作枚举"而不是"转发一段 kubectl 命令"：
//   - 没有命令字符串，就没有命令注入面（与 shell_readonly 用白名单、
//     node_probe 用固定命令表是同一个立场）；
//   - `exec` / `port-forward` / `delete` / `apply` 这些能力**根本不存在**，
//     不是"被过滤掉"——模型无法用一个它没有的工具；
//   - 每个操作返回的都是渲染好的文本 + 证据引用，模型拿不到任意字段，
//     因此 Secret 内容之类的敏感数据不会因为一句"帮我看一下"就进上下文。
//
// 它补上的是一个具体的空缺：admin 侧 Agent 此前只能看到 MySQL + etcd 记录，
// 所以"这个 Pod 为什么起不来"这类问题只能回答"无法确认"。有了它，
// CrashLoopBackOff / ImagePullBackOff / OOMKilled / Pending(资源不足)
// 才可能给出**带证据**的结论，而不是猜测。

// K8sReadSource 只读集群查询通道（由 admin 侧装配）。
//
// 返回 (渲染后的文本, 证据引用列表, error)：
// 引用由数据源生成而不是让模型从文本里抄——引用校验必须是机械的，
// 抄错一个字符就会让"有证据"变成"捏造证据"。
type K8sReadSource interface {
	// Query 执行一次只读查询。
	Query(ctx context.Context, op, namespace, name, container, selector string, tailLines int64, limit int) (string, []string, error)
	// Ops 本数据源支持的只读操作（用于生成工具 Schema 的枚举，
	// 保证"工具说能做什么"与"实现真的能做什么"不会各写一份而漂移）。
	Ops() []string
}

// K8sReadonlyTool 只读集群查询工具。
type K8sReadonlyTool struct {
	src K8sReadSource
	// ops 构造时从数据源固化，保证 Description/Schema/Execute 三处口径一致。
	ops []string
}

// NewK8sReadonlyTool 创建工具。
func NewK8sReadonlyTool(src K8sReadSource) *K8sReadonlyTool {
	t := &K8sReadonlyTool{src: src}
	if src != nil {
		t.ops = src.Ops()
	}
	return t
}

// Name 实现 Tool。
func (t *K8sReadonlyTool) Name() string { return "k8s_readonly" }

// Description 实现 Tool。
func (t *K8sReadonlyTool) Description() string {
	ops := t.ops
	if len(ops) == 0 {
		ops = []string{"pods", "pod_logs", "describe_pod", "events", "deployments", "nodes"}
	}
	return "只读查询 Kubernetes 集群（不执行任何 kubectl 命令，没有 exec/delete/apply 能力）。" +
		"支持的操作：" + strings.Join(ops, "、") + "。" +
		"要断言\"Pod 在崩溃/镜像拉不下来/节点有问题\"这类结论，必须先用本工具实际查到证据；" +
		"查不到就如实说查不到，不要根据经验推测。"
}

// Schema 实现 Tool。
func (t *K8sReadonlyTool) Schema() *jsonschema.Schema {
	ops := t.ops
	if len(ops) == 0 {
		ops = []string{"pods", "pod_logs", "describe_pod", "events", "deployments", "nodes"}
	}
	return jsonschema.Object(map[string]*jsonschema.Schema{
		"op":        jsonschema.String("要执行的操作").WithEnum(stringsToAny(ops)...),
		"namespace": jsonschema.String("命名空间（省略 = 默认命名空间）"),
		"name":      jsonschema.String("Pod / Deployment 名（pod_logs、describe_pod 必填）"),
		"container": jsonschema.String("容器名（pod_logs 用；省略 = main）"),
		"selector":  jsonschema.String("Pod 标签选择器，例如 app=web（pods 用）"),
		"tail_lines": jsonschema.Integer("日志尾部行数（默认 200，上限 2000）").
			WithRange(0, 2000),
		"limit": jsonschema.Integer("列表行数上限（默认 50，上限 200）").WithRange(0, 200),
	}, "op")
}

// ReadOnly 实现 Tool。
func (t *K8sReadonlyTool) ReadOnly() bool { return true }

// Risk 实现 Tool：全部操作都是读；写动作（例如 rollout restart）
// 是**另一个**工具，且有独立的审批闸门，不在这里"顺手提供"。
func (t *K8sReadonlyTool) Risk() RiskLevel { return RiskRead }

// Timeout 实现 Tool：集群查询 + 一次日志拉取，给够时间但不能无限。
func (t *K8sReadonlyTool) Timeout() time.Duration { return 20 * time.Second }

// Execute 实现 Tool。
func (t *K8sReadonlyTool) Execute(ctx context.Context, args map[string]interface{}) (Result, error) {
	if t.src == nil {
		return Result{}, fmt.Errorf("%w: k8s_readonly 数据源未接入（未配置集群）", ErrToolDenied)
	}
	op := strings.ToLower(strings.TrimSpace(llm.StringOr(args, "op", "")))
	if op == "" {
		return Result{OK: false, Summary: "缺少 op 参数", Err: "k8s_readonly 必须指定 op"}, nil
	}
	// 入口再校验一次 op：Schema 是给模型的提示，不是安全边界；
	// 真正的白名单在这里与数据源内部各把一道。
	if !t.opAllowed(op) {
		return Result{OK: false, Summary: "不支持的操作", Err: fmt.Sprintf("op %q 不在允许列表内：%s", op, strings.Join(t.ops, ","))}, nil
	}
	namespace := strings.TrimSpace(llm.StringOr(args, "namespace", ""))
	name := strings.TrimSpace(llm.StringOr(args, "name", ""))
	container := strings.TrimSpace(llm.StringOr(args, "container", ""))
	selector := strings.TrimSpace(llm.StringOr(args, "selector", ""))
	tailLines := int64(llm.FloatOr(args, "tail_lines", 0))
	limit := int(llm.FloatOr(args, "limit", 0))

	// 缺参数就地拒绝：多一次集群往返不会让结果更好，只会让模型以为"查过了"。
	if (op == "pod_logs" || op == "describe_pod") && name == "" {
		return Result{OK: false, Summary: "缺少 name 参数", Err: fmt.Sprintf("%s 必须指定 name（Pod 名）", op)}, nil
	}

	text, refs, err := t.src.Query(ctx, op, namespace, name, container, selector, tailLines, limit)
	if err != nil {
		// 工具失败是**观测**：模型需要看到"这次查询失败了"才能改变策略，
		// 而不是让整轮会话以异常结束。
		return Result{OK: false, Summary: fmt.Sprintf("集群查询失败（op=%s）", op), Err: llm.Redact(err.Error())}, nil
	}

	evidence := make([]Evidence, 0, 5)
	for i, ref := range refs {
		if i >= 5 {
			break
		}
		evidence = append(evidence, Evidence{Source: "k8s", Ref: ref, Quote: firstLines(text, 3)})
	}
	res := Result{
		OK:       true,
		Output:   text,
		Summary:  fmt.Sprintf("k8s %s 查询完成（命名空间=%q 目标=%q 证据 %d 条）", op, firstNonEmpty(namespace, "默认"), name, len(evidence)),
		Evidence: evidence,
		Meta:     map[string]interface{}{"op": op, "refs": len(refs)},
	}
	if strings.TrimSpace(text) == "" {
		res.Summary = fmt.Sprintf("k8s %s 查询返回空结果——这本身是证据（可能是选择器/命名空间写错，或该对象不存在）", op)
	}
	return res, nil
}

func (t *K8sReadonlyTool) opAllowed(op string) bool {
	if len(t.ops) == 0 {
		return false
	}
	for _, o := range t.ops {
		if strings.EqualFold(strings.TrimSpace(o), op) {
			return true
		}
	}
	return false
}

// firstLines 复用 builtin.go 里的实现（同一份引文口径，避免两处截断方式不一致）。

var _ Tool = (*K8sReadonlyTool)(nil)
