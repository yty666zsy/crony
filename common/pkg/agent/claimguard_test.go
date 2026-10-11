package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
)

// 这些用例的文本**逐字取自真实联调**（2026-10-11，qwen-plus，播种失败用例
// j910005-m1791721594775539400-1），不是编造的样例。

// 真实原文（v2/v4）：summary 把"未监听"写成既定结论。
const liveAssertiveSummary = "任务 910005 失败的最可能原因是目标 MySQL 实例未在 node-it-01 上监听 127.0.0.1:13307，导致 PowerShell 脚本连接被主动拒绝（connection refused），置信度 85%"

// 真实原文（v4）：cause 里同样是断言句。
const liveAssertiveCause = "PowerShell 脚本 check_db_port.ps1 尝试连接 MySQL 服务时收到 'No connection could be made because the target machine actively refused it 127.0.0.1:13307'，表明 TCP 连接请求被目标主机明确拒绝（RST 包），通常因端口无进程监听或防火墙拦截"

// TestClaimGuardFlagsLiveUnobservedPortClaim 守卫必须抓住真实联调里的那句断言。
func TestClaimGuardFlagsLiveUnobservedPortClaim(t *testing.T) {
	res := CheckNodeSideClaims(liveAssertiveSummary+"\n"+liveAssertiveCause,
		[]string{"job_run_step:j910005-m1791721594775539400-1#0", "job:910005"}, nil)
	if !res.Downgraded || len(res.Claims) == 0 {
		t.Fatalf("未观测的节点侧断言必须被标记: %+v", res)
	}
	joined := strings.Join(res.Claims, ",")
	if !strings.Contains(joined, "端口未监听") {
		t.Fatalf("应命中\"端口未监听\"，实际 %v", res.Claims)
	}
}

// TestClaimGuardIgnoresHedgedWording 已标注为推测/待确认的措辞不得被拦——
// 否则守卫会逼模型连推测都不敢说，反而降低诊断价值。
func TestClaimGuardIgnoresHedgedWording(t *testing.T) {
	for _, text := range []string{
		"连接 127.0.0.1:13307 被拒绝，疑似目标实例未在该节点监听（需在节点确认）",
		"推测该节点上的 MySQL 服务未运行，尚未验证",
		"端口可能未监听（假设，不排除防火墙拦截）",
	} {
		res := CheckNodeSideClaims(text, nil, nil)
		if res.Downgraded {
			t.Fatalf("带推测措辞的文本不应被标记: %q -> %v", text, res.Claims)
		}
	}
}

// TestClaimGuardExemptsWhenNodeEvidenceCited 引用了节点侧观测证据的结论放行。
func TestClaimGuardExemptsWhenNodeEvidenceCited(t *testing.T) {
	observed := []tools.Evidence{
		{Source: "job_run", Ref: "job_run:r1", Quote: "status=failed"},
		{Source: "node", Ref: "node-it-01", Quote: "online=在线 last_heartbeat=... metrics=未上报"},
	}
	res := CheckNodeSideClaims(liveAssertiveSummary, []string{"node-it-01"}, observed)
	if res.Downgraded {
		t.Fatalf("引用了节点侧证据不应被标记: %+v", res)
	}
	if !res.NodeObserved {
		t.Fatalf("应识别出存在节点侧观测: %+v", res)
	}
}

// TestClaimGuardAcceptsNodeProbeEvidence 阶段 3 第二段的关键衔接：
// node_probe 工具拿到的证据必须被认作"节点侧观测"。
//
// 两条增量必须拼得上：探测通道负责**真的去问节点**，结论句守卫负责
// **拦住没问就断言的结论**。如果 node_probe 的证据不算节点侧观测，
// 那模型即使老老实实探测了，结论仍会被标记为"未观测"——通道白建。
func TestClaimGuardAcceptsNodeProbeEvidence(t *testing.T) {
	observed := []tools.Evidence{
		{Source: "job_run", Ref: "job_run:j910005-m1", Quote: "status=failed"},
		{Source: "node_probe", Ref: "node_probe:pnode-it-01-1791721-1",
			Quote: "netstat -ano -> TCP 0.0.0.0:13307 LISTENING\nTCP 127.0.0.1:13306 LISTENING"},
	}
	refs := []string{"job_run:j910005-m1", "node_probe:pnode-it-01-1791721-1"}
	res := CheckNodeSideClaims(liveAssertiveSummary, refs, observed)
	if res.Downgraded {
		t.Fatalf("引用了 node_probe 观测证据的结论不应被标记: %+v", res)
	}
	if !res.NodeObserved {
		t.Fatalf("node_probe 应被识别为节点侧观测: %+v", res)
	}
}

// TestClaimGuardFlagsWhenProbeWasRunButNotCited 探测跑了但结论没引用它——
// 依然算"未观测就断言"（也说明模型可能根本没看探测结果）。
func TestClaimGuardFlagsWhenProbeWasRunButNotCited(t *testing.T) {
	observed := []tools.Evidence{
		{Source: "node_probe", Ref: "node_probe:p1", Quote: "netstat -ano -> TCP ..."},
	}
	res := CheckNodeSideClaims(liveAssertiveSummary, []string{"job:910005"}, observed)
	if !res.Downgraded {
		t.Fatalf("未引用探测证据的断言必须被标记: %+v", res)
	}
}

// TestClaimGuardStillFlagsWhenProbeFoundNothing 探测执行了、但输出里没有监听信息——
// 依然不能断言"未监听"：netstat 没看到可能是命令被拒、输出被截断或看错端口。
// 只有**明确观测到否定事实**才算观测到（防止"跑一次工具就获得任意断言许可"）。
func TestClaimGuardStillFlagsWhenProbeFoundNothing(t *testing.T) {
	observed := []tools.Evidence{
		{Source: "node_probe", Ref: "node_probe:p2",
			Quote: "netstat -ano -> [error] 命令被节点侧白名单拒绝（未执行）"},
	}
	res := CheckNodeSideClaims(liveAssertiveSummary, []string{"node_probe:p2"}, observed)
	if !res.Downgraded {
		t.Fatalf("探测失败/无输出时不得据此断言: %+v", res)
	}
}

// TestClaimGuardFlagsEvenWhenNodeObservedButNotCited 调了 node_status 却没用它支撑结论——
// 结论依然没有依据，必须标记（"调了工具"不等于"结论有据"）。
func TestClaimGuardFlagsEvenWhenNodeObservedButNotCited(t *testing.T) {
	observed := []tools.Evidence{{Source: "node", Ref: "node-it-01", Quote: "online=在线"}}
	res := CheckNodeSideClaims(liveAssertiveSummary, []string{"job:910005"}, observed)
	if !res.Downgraded {
		t.Fatalf("未引用观测证据的断言应被标记: %+v", res)
	}
	if !res.NodeObserved {
		t.Fatalf("NodeObserved 应为 true: %+v", res)
	}
}

// TestClaimGuardClauseLevelExemption 同一段文本里"某句是推测"不能给整段免责。
func TestClaimGuardClauseLevelExemption(t *testing.T) {
	text := "推测磁盘可能已满；服务未运行导致连接被拒"
	res := CheckNodeSideClaims(text, nil, nil)
	if !res.Downgraded {
		t.Fatalf("第二句是断言，必须被标记: %+v", res)
	}
	joined := strings.Join(res.Claims, ",")
	if !strings.Contains(joined, "服务未运行") {
		t.Fatalf("应命中\"服务未运行\"，实际 %v", res.Claims)
	}
	if strings.Contains(joined, "磁盘已满") {
		t.Fatalf("\"推测磁盘可能已满\"这一句不该被标记，实际 %v", res.Claims)
	}
}

// TestClaimGuardPatternCoverage 覆盖各类节点侧断言。
func TestClaimGuardPatternCoverage(t *testing.T) {
	cases := map[string]string{
		"端口未监听":       "该主机 3306 端口未监听",
		"服务未运行":       "目标机器上的 mysqld 服务未运行",
		"磁盘已满":        "节点磁盘空间不足，日志写入失败",
		"内存/CPU 资源耗尽": "节点内存耗尽导致进程被 OOM 杀死",
		"进程不存在":       "备份进程不存在",
		"文件/目录不存在":    "备份目录不存在",
		"防火墙拦截":       "节点防火墙拦截了入站连接",
	}
	for want, text := range cases {
		res := CheckNodeSideClaims(text, nil, nil)
		if !res.Downgraded {
			t.Fatalf("%q 未被识别: %q", want, text)
		}
		if !strings.Contains(strings.Join(res.Claims, ","), want) {
			t.Fatalf("%q 识别到的类型不符: %v", want, res.Claims)
		}
	}
}

// TestClaimGuardEmptyTextIsSafe 空结论文本不得 panic 也不得误报。
func TestClaimGuardEmptyTextIsSafe(t *testing.T) {
	if res := CheckNodeSideClaims("", nil, nil); res.Downgraded {
		t.Fatalf("空文本不应被标记: %+v", res)
	}
	if res := CheckNodeSideClaims("   \n  ", nil, nil); res.Downgraded {
		t.Fatalf("空白文本不应被标记: %+v", res)
	}
}

// TestDiagnoseDowngradesUnobservedNodeClaim 端到端：守卫必须接进 Diagnose，
// 而不是只作为可调用的工具函数存在。
//
// 场景完全复刻真实联调：模型调了 log_search（证据 ref 真实、grounding 通过），
// 但结论句断言了"未在节点上监听 13307"——ref 全真，结论越界。
func TestDiagnoseDowngradesUnobservedNodeClaim(t *testing.T) {
	gateway, fp := newGateway()
	src := &fakeLogSource{hits: []tools.LogHit{
		{Source: "node.log", Ref: "node.log:12", Line: "actively refused it 127.0.0.1:13307", Timestamp: time.Now()},
	}}
	fp.WithResponder(func(req llm.Request) (llm.Completion, error) {
		for _, m := range req.Messages {
			if m.Role == llm.RoleTool {
				payload := validDiagnosis("node.log:12", true)
				payload["summary"] = liveAssertiveSummary
				payload["cause"] = liveAssertiveCause
				return llm.FakeJSON(payload), nil
			}
		}
		return llm.FakeToolCall("log_search", map[string]interface{}{"keyword": "13307", "job_id": 42}), nil
	})
	a := New(gateway, newLogRegistry(t, src), DefaultConfig())

	diag, err := Diagnose(context.Background(), a, failedRunContext())
	if err != nil {
		t.Fatalf("应有结论: %v", err)
	}
	if len(diag.UnverifiedNodeClaims) == 0 {
		t.Fatalf("结论里的节点侧断言必须被标记: %+v", diag)
	}
	if !strings.Contains(strings.Join(diag.UnverifiedNodeClaims, ","), "端口未监听") {
		t.Fatalf("应命中\"端口未监听\"，实际 %v", diag.UnverifiedNodeClaims)
	}
	if diag.EvidenceSufficient {
		t.Fatal("含未观测节点侧断言的结论必须强制降级为证据不足")
	}
	found := false
	for _, w := range diag.Warnings {
		if strings.Contains(w, "未观测的节点侧事实断言") {
			found = true
		}
	}
	if !found {
		t.Fatalf("必须给出告警，实际 %v", diag.Warnings)
	}
	if len(diag.NextDataToCollect) == 0 {
		t.Fatal("降级后必须给出待收集清单")
	}
	if !strings.Contains(diag.Explain(), "未观测的节点侧断言") {
		t.Fatalf("人话报告必须显示该告警:\n%s", diag.Explain())
	}
}
