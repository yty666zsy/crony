package request

import (
	"encoding/json"
	"testing"
)

// 控制台检索请求体的**绑定契约**（不是 SQL，而是"前端传来的 JSON 能不能落到字段上"）。
//
// 这两条都是"handler 编译通过、逻辑也没错，但字段永远收不到值"的典型：
// 绑定层的错法全是静默的——请求体里明明有，结构体里却是零值。
//
//  1. ReqNodeSearch 的 status 标签曾误写成 `son:"status"`（少了 j），
//     hostname 则压根没有对应字段；
//  2. ReqJobSearch 的 status 是 int，而未分配状态的枚举值就是 0，
//     于是"筛选未分配"与"不筛选"在类型上无法区分 —— 必须用指针。
func TestReqNodeSearchBindsHostnameAndStatus(t *testing.T) {
	var req ReqNodeSearch
	body := []byte(`{"hostname":"node-a","ip":"10.1.2.3","uuid":"u-1","status":2,"page":1,"page_size":10}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("节点检索请求体解析失败: %v", err)
	}
	if req.Hostname != "node-a" {
		t.Fatalf("hostname 未绑定到字段（前端传了却收不到 = 筛选静默失效）: %q", req.Hostname)
	}
	if req.Status != 2 {
		t.Fatalf("status 未绑定到字段（标签少写 j 也不会报错，只会收到零值）: %d", req.Status)
	}
	if req.IP != "10.1.2.3" || req.UUID != "u-1" {
		t.Fatalf("ip/uuid 绑定异常: %+v", req)
	}
}

func TestReqJobSearchStatusNilMeansNoFilter(t *testing.T) {
	// 不传 status：必须是 nil（= 不筛选），不能退化成 0（= 只看未分配）。
	var noFilter ReqJobSearch
	if err := json.Unmarshal([]byte(`{"name":"x"}`), &noFilter); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if noFilter.Status != nil {
		t.Fatalf("不传 status 时必须为 nil（不筛选），实际 %v", *noFilter.Status)
	}

	// 传 status=0：必须能区分出"筛选未分配"。
	var zero ReqJobSearch
	if err := json.Unmarshal([]byte(`{"status":0}`), &zero); err != nil {
		t.Fatalf("解析 status=0 失败: %v", err)
	}
	if zero.Status == nil || *zero.Status != 0 {
		t.Fatalf("status=0 必须绑定为指向 0 的指针（否则\"筛选未分配\"永远不生效）: %v", zero.Status)
	}

	var one ReqJobSearch
	if err := json.Unmarshal([]byte(`{"status":1}`), &one); err != nil {
		t.Fatalf("解析 status=1 失败: %v", err)
	}
	if one.Status == nil || *one.Status != 1 {
		t.Fatalf("status=1 绑定异常: %v", one.Status)
	}
}
