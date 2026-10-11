package agent

import (
	"regexp"
	"strings"

	"github.com/tmnhs/crony/common/pkg/tools"
)

// 本文件实现"结论句守卫"（claim guard）。
//
// 为什么需要它——这是一次真实联调逼出来的机制，不是设想：
// 接入 node_status 工具后重跑同一个播种失败用例，模型**一次工具都没调**，
// 却在 summary 里断言"127.0.0.1:13307 未在 node-it-01 上监听"。
// 这类事实只有目标机器自己知道，admin 侧不可能观测到；模型是从
// "连接被主动拒绝"推测出来的，但它把推测写成了既成结论。
//
// 提示词能改善措辞（v3 让它把"需在节点确认"写进 next_data_to_collect、
// v4 让 assumptions 填上了未验证假设），**但交叉实测三次都不能阻止 summary
// 把最可能原因写成断言句**。所以这里改为机械校验：
//
//	结论句里出现"节点侧事实断言" + 本次运行没有任何节点侧观测证据
//	→ 标记 unverified_node_claims、强制 evidence_sufficient=false、补一条待收集项
//
// 设计取向是"宁可多标一句，不可少标一句"：误标只会让报告多一条提示，
// 漏标则可能让人照着未观测的结论去改生产配置。

// nodeObservedSourcePrefixes 视为"节点侧观测"的证据来源。
//
// 目前只有 node_status 工具产出的 source=node；节点侧 shell_readonly
// （阶段 3 后续的只读探针通道）落地后应一并加进来。
var nodeObservedSourcePrefixes = []string{"node"}

// nodeClaim 一类节点侧事实断言的检测规则。
type nodeClaim struct {
	// Label 是给人看的断言类型名（写进告警文本）。
	Label string
	re    *regexp.Regexp
}

// nodeClaimRules 检测"只有目标机器自己知道"的事实断言。
//
// 规则刻意收窄到高置信的措辞（"未监听""未启动""已满"这类完成态断言），
// 不匹配"疑似/可能/需确认"的措辞——推测本来就该允许存在，
// 我们只拦"把推测写成事实"。正则里的 (?s) 让 . 可跨行。
var nodeClaimRules = []nodeClaim{
	// 端口未监听：真实联调原文是"未在 node-it-01 上监听 127.0.0.1:13307"——
	// **没有出现"端口"二字**，所以规则必须覆盖"未(在)…监听"这种写法，
	// 只匹配"端口…监听"会漏掉本次实测里最典型的那句断言。
	{Label: "端口未监听", re: regexp.MustCompile(`(?s)(未|没有|不在|未在)[^。；\n]{0,24}监听|未监听|端口[^。；\n]{0,20}监听|(port|端口)[^。；\n]{0,12}not listening`)},
	{Label: "服务未运行", re: regexp.MustCompile(`(?s)(服务|进程|实例|mysqld|mysql|nginx|redis|docker)[^。；\n]{0,16}(未|没有|不在|已)?(运行|启动|处于运行)|服务已停止|进程未启动`)},
	{Label: "磁盘已满", re: regexp.MustCompile(`(?s)磁盘[^。；\n]{0,12}(已满|满了|空间不足|写满|不足)|磁盘空间不足|no space left`)},
	{Label: "内存/CPU 资源耗尽", re: regexp.MustCompile(`(?s)(内存|memory)[^。；\n]{0,12}(不足|耗尽|打满|溢出)|OOM|(CPU|cpu)[^。；\n]{0,12}(过高|打满|满载|100%)`)},
	{Label: "进程不存在", re: regexp.MustCompile(`(?s)进程[^。；\n]{0,12}(不存在|未找到|没有|缺失)`)},
	{Label: "文件/目录不存在", re: regexp.MustCompile(`(?s)(文件|目录|脚本)[^。；\n]{0,12}(不存在|未找到|已丢失|缺失)`)},
	{Label: "防火墙拦截", re: regexp.MustCompile(`(?s)(防火墙|firewall|安全组)[^。；\n]{0,12}(拦截|阻断|阻止|拒绝)`)},
}

// nodeClaimExemptPatterns 表示"模型已经把它当推测说"的措辞。
//
// 出现这些词时不标记：规则的目标是拦"断言"，不是拦"推测"。
//
// 刻意**不包含**"可能"：本项目的模型习惯用"最可能原因是……"引出主结论
// （真实联调里就是"最可能原因是目标 MySQL 实例未在节点上监听……"），
// 把"可能"当免责词会让守卫在最需要它的场景下失效。
var nodeClaimExemptPatterns = []*regexp.Regexp{
	regexp.MustCompile(`疑似`), regexp.MustCompile(`或许`),
	regexp.MustCompile(`推测`), regexp.MustCompile(`假设`), regexp.MustCompile(`未验证`),
	regexp.MustCompile(`待确认`), regexp.MustCompile(`不排除`), regexp.MustCompile(`需(要)?(在)?(节点|主机|机器)上?确认`),
	regexp.MustCompile(`看起来`), regexp.MustCompile(`倾向于`), regexp.MustCompile(`suspected|likely|possibly`),
}

// claimClauseSeps 把结论文本切成"子句"再逐句评估。
//
// 为什么必须逐句：一句话里出现"疑似"不能给整段话免责——
// 真实联调里 summary 是断言句、next_data_to_collect 才是"需在节点确认"，
// 如果把整段文本当一个整体判豁免，守卫会被无关的措辞绕过。
var claimClauseSeps = regexp.MustCompile(`[。；;\n，,]+`)

// NodeClaimGuardResult 守卫结果。
type NodeClaimGuardResult struct {
	// Claims 命中的节点侧断言类型（去重，例如 ["端口未监听","服务未运行"]）。
	Claims []string
	// NodeObserved 本次运行是否存在节点侧观测证据。
	NodeObserved bool
	// Downgraded 是否因此把结论降级（证据不足 + 告警 + 待收集项）。
	Downgraded bool
}

// CheckNodeSideClaims 检查结论文本里是否出现"未观测的节点侧事实断言"。
//
// text 一般传 summary + cause；cited 传模型给出的证据 ref；
// observed 传本次运行真实观测到的证据（tools.Evidence，含 Source）。
// 只有**引用到的**节点侧证据才算"观测过"：调了 node_status
// 却没用它的 ref 支撑结论，结论依然没有依据。
func CheckNodeSideClaims(text string, cited []string, observed []tools.Evidence) NodeClaimGuardResult {
	res := NodeClaimGuardResult{}
	nodeRefs := map[string]bool{}
	for _, e := range observed {
		if !isNodeObservedSource(e.Source) || strings.TrimSpace(e.Ref) == "" {
			continue
		}
		res.NodeObserved = true
		nodeRefs[strings.ToLower(strings.TrimSpace(e.Ref))] = true
	}
	// 结论引用了节点侧证据 → 这条结论有节点侧观测支撑，不拦。
	for _, ref := range cited {
		if refObserved(nodeRefs, ref) {
			return res
		}
	}

	seen := map[string]bool{}
	for _, clause := range claimClauseSeps.Split(text, -1) {
		clause = strings.TrimSpace(clause)
		if clause == "" || hasExemptWording(clause) {
			continue
		}
		for _, rule := range nodeClaimRules {
			if seen[rule.Label] || !rule.re.MatchString(clause) {
				continue
			}
			seen[rule.Label] = true
			res.Claims = append(res.Claims, rule.Label)
		}
	}
	res.Downgraded = len(res.Claims) > 0
	return res
}

// hasExemptWording 判断该子句是否已用推测措辞表达。
func hasExemptWording(clause string) bool {
	for _, ex := range nodeClaimExemptPatterns {
		if ex.MatchString(clause) {
			return true
		}
	}
	return false
}

func isNodeObservedSource(source string) bool {
	s := strings.ToLower(strings.TrimSpace(source))
	for _, prefix := range nodeObservedSourcePrefixes {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

// applyNodeClaimGuard 把守卫结果落到结论上（就地修改）。
//
// 只降级、不改写模型原文：改写会掩盖"模型说了不该说的话"这个事实，
// 而运维报告里"作者到底说了什么"本身就是要留痕的。
func applyNodeClaimGuard(diag *Diagnosis, observed []tools.Evidence) {
	text := strings.TrimSpace(diag.Summary + "\n" + diag.Cause)
	if text == "" {
		return
	}
	cited := make([]string, 0, len(diag.Evidence))
	for _, e := range diag.Evidence {
		cited = append(cited, e.Ref)
	}
	res := CheckNodeSideClaims(text, cited, observed)
	if !res.Downgraded {
		return
	}
	diag.UnverifiedNodeClaims = res.Claims
	diag.EvidenceSufficient = false
	diag.Warnings = append(diag.Warnings, strings.Join([]string{
		"结论句包含未观测的节点侧事实断言（" + strings.Join(res.Claims, "、") + "）：",
		"这类事实只有目标机器自己知道，本次运行没有相应的节点侧观测证据。",
		"已强制标记为证据不足——请先在节点上确认（node_status / 节点侧只读探测）再据此决策。",
	}, ""))
	if len(diag.NextDataToCollect) == 0 {
		diag.NextDataToCollect = append(diag.NextDataToCollect,
			"在目标节点上观测上述断言涉及的事实（端口监听状态 / 服务运行状态 / 资源水位 / 文件与进程是否存在）")
	}
}
