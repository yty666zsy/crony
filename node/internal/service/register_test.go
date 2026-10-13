package service

import "testing"

// 20 节点舰队重启实测暴露的根因回归：
//
// `exist()` 曾把 etcd 节点键的值当**整数 PID** 解析，而 `Register()` 写入的是 **Node JSON**。
// 于是解析必然失败，旧实现顺手把这个节点键**删掉** —— 伪造出一次"节点失联"：
//   - 一个健康节点上的任务被无谓 FailOver 到别的节点；
//   - admin 按下线处理并删掉该节点的库内行，节点随即重新注册 →
//     "etcd 里有它、库里没有它"的漂移（节点页/统计/DB 侧调度都看不见它）。
//
// `registeredPID` 把"取值格式"收敛到一处，并明确口径：**解析不出来 = 不确定**，
// 调用方不得据此删键。
func TestRegisteredPIDParsesNodeJSON(t *testing.T) {
	val := []byte(`{"id":1,"pid":"4321","uuid":"u-1","status":1}`)
	pid, ok := registeredPID(val)
	if !ok || pid != 4321 {
		t.Fatalf("registeredPID(Node JSON) = %d,%v，期望 4321,true", pid, ok)
	}
}

// 兼容更早的实现：值直接是裸 PID 字符串。
func TestRegisteredPIDAcceptsLegacyBarePID(t *testing.T) {
	if pid, ok := registeredPID([]byte("12345")); !ok || pid != 12345 {
		t.Fatalf("registeredPID(裸 PID) = %d,%v，期望 12345,true", pid, ok)
	}
}

func TestRegisteredPIDRejectsUnparsableValues(t *testing.T) {
	cases := map[string][]byte{
		"空值":          []byte{},
		"乱码":          []byte("not-a-node"),
		"JSON 但无 pid": []byte(`{"uuid":"u-1"}`),
		"pid 非数字":     []byte(`{"pid":"abc"}`),
		"pid 为 0":     []byte(`{"pid":"0"}`),
		"pid 为负数":     []byte(`{"pid":"-3"}`),
		"JSON 数组":     []byte(`[1,2,3]`),
	}
	for name, val := range cases {
		if pid, ok := registeredPID(val); ok {
			t.Fatalf("%s：不得解析出 PID（实际 %d）——否则会据此误判『有人在跑』或删键", name, pid)
		}
	}
}
