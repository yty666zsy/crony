package request

import (
	"strings"
	"testing"

	"github.com/tmnhs/crony/common/models"
)

// TestReqJobUpdateValidRejectsFiveSegmentSpec 是本包新增的入口拦截：
// 5 段 cron 在本系统调度库里是合法表达式，但语义是「秒 分 时 日 月」
// （"0 3 * * *" = 每小时第 3 分钟），不报错、只按错频率跑。
// 必须在保存这一层拒绝，并把原因说清楚（段数 + 真实语义 + 正确写法）。
func TestReqJobUpdateValidRejectsFiveSegmentSpec(t *testing.T) {
	req := &ReqJobUpdate{
		Job: &models.Job{
			Name:    "每天三点清理",
			Command: "echo clean",
			Spec:    "0 3 * * *",
		},
	}
	err := req.Valid()
	if err == nil {
		t.Fatal("5 段 cron 必须在 /job/add 入口被拒绝（否则会静默变成每小时任务）")
	}
	msg := err.Error()
	for _, want := range []string{"5 段", "6 段", "每小时第 3 分钟"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错应包含 %q，实际：%s", want, msg)
		}
	}
}

// TestReqJobUpdateValidAcceptsSixSegmentSpec 正常任务不能被误伤：
// 6 段表达式、描述符、以及默认的自动分配都要照常通过。
func TestReqJobUpdateValidAcceptsSixSegmentSpec(t *testing.T) {
	for _, spec := range []string{"0 0 3 * * *", "*/30 * * * * *", "@daily"} {
		req := &ReqJobUpdate{
			Job: &models.Job{
				Name:    "正常任务",
				Command: "echo ok",
				Spec:    spec,
			},
		}
		if err := req.Valid(); err != nil {
			t.Errorf("spec=%q 应当通过校验，实际 %v", spec, err)
		}
		if req.Allocation != models.AutoAllocation {
			t.Errorf("allocation=0 应被兜底成自动分配，实际 %d", req.Allocation)
		}
	}
}
