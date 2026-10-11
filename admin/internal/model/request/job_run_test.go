package request

import (
	"strings"
	"testing"
)

// TestValidConcurrencyPolicy 并发策略入口校验：
// 非法值必须在保存时被拒绝，而不是让节点静默回退成 skip。
func TestValidConcurrencyPolicy(t *testing.T) {
	for _, valid := range []int{0, 1, 2, 3, 4} {
		if err := ValidConcurrencyPolicy(valid); err != nil {
			t.Fatalf("策略 %d 应当合法，实际报错: %v", valid, err)
		}
	}
	for _, invalid := range []int{-1, 5, 99, 1000} {
		err := ValidConcurrencyPolicy(invalid)
		if err == nil {
			t.Fatalf("策略 %d 应当被拒绝", invalid)
		}
		if !strings.Contains(err.Error(), "并发策略") {
			t.Fatalf("错误信息应说明是并发策略问题，实际 %q", err.Error())
		}
	}
}

// TestReqJobRunSearchCheck 分页参数兜底 + PageSize 上限（防止被放大成拖库查询）。
func TestReqJobRunSearchCheck(t *testing.T) {
	req := &ReqJobRunSearch{}
	req.Check()
	if req.Page != 1 || req.PageSize != 20 {
		t.Fatalf("默认分页不符: page=%d size=%d", req.Page, req.PageSize)
	}

	req = &ReqJobRunSearch{PageInfo: PageInfo{Page: 3, PageSize: 1000}}
	req.Check()
	if req.Page != 3 || req.PageSize != 200 {
		t.Fatalf("PageSize 应被压到 200: page=%d size=%d", req.Page, req.PageSize)
	}

	steps := &ReqJobRunSteps{PageInfo: PageInfo{PageSize: 5000}}
	steps.Check()
	if steps.PageSize != 500 || steps.Page != 1 {
		t.Fatalf("步骤分页兜底不符: page=%d size=%d", steps.Page, steps.PageSize)
	}
}
