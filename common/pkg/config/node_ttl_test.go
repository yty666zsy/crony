package config

import "testing"

// 20 节点舰队实测（F6）暴露：node-ttl 直接进 etcd 的 LeaseGrant，
// 而**租约过期就是 FailOver 的唯一触发器**。所以这里有两条不能含糊的口径：
//  1. 未配置/非法（<=0）必须回退到保守默认 —— etcd 对 ttl<=0 会退回最小租约（约 1s），
//     那会让节点被反复判死、反复 FailOver，属于"集群自己打自己"的配置错误；
//  2. 用户显式配的激进值必须**被尊重**（不静默改写），但必须给出告警。
func TestResolveNodeTtlFallsBackWhenUnsetOrInvalid(t *testing.T) {
	for _, in := range []int64{0, -1, -3600} {
		ttl, warning := ResolveNodeTtl(in)
		if ttl != DefaultNodeTtl {
			t.Fatalf("configured=%d 必须回退到保守默认 %d（etcd 会对 ttl<=0 用最小租约），实际 %d",
				in, DefaultNodeTtl, ttl)
		}
		if warning == "" {
			t.Fatalf("configured=%d 发生兜底时必须告警（否则运维不知道自己在跑默认值）", in)
		}
	}
}

func TestResolveNodeTtlHonoursExplicitValue(t *testing.T) {
	// 显式配置（含等于下限、明显保守的值）一律照用。
	for _, in := range []int64{MinNodeTtl, MinNodeTtl + 1, 30, 300} {
		ttl, warning := ResolveNodeTtl(in)
		if ttl != in {
			t.Fatalf("显式配置 configured=%d 必须被尊重（不静默改写），实际 %d", in, ttl)
		}
		if warning != "" {
			t.Fatalf("configured=%d 不低于下限，不应告警，实际告警 %q", in, warning)
		}
	}
}

func TestResolveNodeTtlWarnsButKeepsAggressiveValue(t *testing.T) {
	ttl, warning := ResolveNodeTtl(MinNodeTtl - 1)
	if ttl != MinNodeTtl-1 {
		t.Fatalf("过激进的显式值也必须照用，实际 %d", ttl)
	}
	if warning == "" {
		t.Fatalf("低于 MinNodeTtl(%d) 必须告警：多节点同机部署下会把健康节点误判为失联", MinNodeTtl)
	}
}
