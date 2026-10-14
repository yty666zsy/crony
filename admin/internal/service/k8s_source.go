package service

import (
	"context"
	"fmt"

	"github.com/tmnhs/crony/common/pkg/k8sclient"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// k8sReadSource 把 k8sclient 适配成 tools.K8sReadSource。
//
// 为什么中间要有一层适配而不是让 tools 直接依赖 k8sclient：
// tools 是安全网关，它只该知道"有一次只读查询 + 一段文本 + 若干证据引用"，
// 不该知道 Pod/Deployment/Node 这些 k8s 概念；反过来 k8sclient 也不该知道
// 工具网关的白名单与审计。两边通过这个窄接口对接，
// 于是"新增一个只读操作"只改 k8sclient，而"网关策略"的变化不会碰集群代码。
type k8sReadSource struct {
	client *k8sclient.Client
}

// NewK8sReadSource 构造只读数据源（client 为 nil 时返回 nil，调用方据此不注册工具）。
func NewK8sReadSource(client *k8sclient.Client) tools.K8sReadSource {
	if client == nil || !client.Enabled() {
		return nil
	}
	return &k8sReadSource{client: client}
}

// Ops 支持的只读操作（Schema 枚举直接取它，避免两处各写一份而漂移）。
func (s *k8sReadSource) Ops() []string { return k8sclient.ReadOps() }

// Query 执行一次只读查询。
func (s *k8sReadSource) Query(ctx context.Context, op, namespace, name, container, selector string, tailLines int64, limit int) (string, []string, error) {
	parsed, ok := k8sclient.ParseReadOp(op)
	if !ok {
		return "", nil, fmt.Errorf("不支持的只读操作 %q", op)
	}
	res, err := s.client.Read(ctx, k8sclient.ReadRequest{
		Op:        parsed,
		Namespace: namespace,
		Name:      name,
		Container: container,
		Selector:  selector,
		TailLines: tailLines,
		Limit:     limit,
	})
	if err != nil {
		return "", nil, err
	}
	return res.Text, res.Refs, nil
}
