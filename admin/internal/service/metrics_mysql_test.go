//go:build integration

package service

import (
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
)

// 真实 MySQL 下的指标看板口径测试（PHASE18）。
//
// 为什么必须用真库：`Overview()` 里的错**不会**出现在纯函数里，而会出现在 SQL 的这一侧 ——
//
//	① 忘了加窗口条件（"近 7 天"变成"全部"）；
//	② 把 denied/planned 也算进分母；
//	③ MTTR 的 `finished_at - created` 取错（例如按 created 排序取第一条、或者把未终结行算进来）。
//
// 这三条都只能靠"造夹具 → 看接口 → 与库对账"来抓。
//
// 运行：设置 CRONY_TEST_MYSQL_DSN 并加 -tags integration；未设置则跳过。
func TestIntegrationMetricsOverviewAgainstRealMySQL(t *testing.T) {
	dsn := os.Getenv("CRONY_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 的指标看板口径测试")
	}
	if _, err := dbclient.Init(dsn, "silent", 1, 5); err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db := dbclient.GetMysqlDB()
	if err := db.AutoMigrate(&models.JobHeal{}); err != nil {
		t.Fatalf("建表失败（测试库需要 CREATE 权限）: %v", err)
	}

	tag := fmt.Sprintf("it-metrics-%d", time.Now().UnixNano())
	now := time.Now().Unix()
	defer func() {
		db.Exec("DELETE FROM "+models.CronyJobHealTableName+" WHERE source_run_id LIKE ?", tag+"%")
	}()

	// 夹具（全都在窗口内，除最后一条）：
	//   3 条 succeeded（耗时 10/20/30 秒）+ 1 条 escalated + 1 条 denied + 1 条 planned
	//   + 1 条**窗口外**的 succeeded（40 天前）
	// 期望：succeeded 只 +3（窗口外那条不算）、decided 只 +4（denied/planned 不进分母）。
	type fixture struct {
		status   models.HealStatus
		duration int64 // 秒
		ageDays  int64 // 创建于多少天前
	}
	fixtures := []fixture{
		{models.HealStatusSucceeded, 10, 0},
		{models.HealStatusSucceeded, 20, 0},
		{models.HealStatusSucceeded, 30, 0},
		{models.HealStatusEscalated, 0, 0},
		{models.HealStatusDenied, 0, 0},
		{models.HealStatusPlanned, 0, 0},
		{models.HealStatusSucceeded, 5, 40}, // 窗口外
	}

	before := DefaultMetricsService.Overview()
	for i, f := range fixtures {
		created := now - f.ageDays*86400
		finished := int64(0)
		if f.status == models.HealStatusSucceeded {
			// 窗口外那条也要有 finished_at，否则"被排除"可能只是因为没终结，
			// 断言就失去了区分窗口过滤的能力。
			finished = created + f.duration
		}
		heal := &models.JobHeal{
			HealID:      fmt.Sprintf("%s-%d", tag, i),
			SourceRunID: fmt.Sprintf("%s-run%d", tag, i),
			JobID:       900000 + i,
			JobName:     "it-metrics",
			Attempt:     1,
			Symptom:     "it-metrics fixture",
			Status:      f.status,
			Action:      models.HealActionRetrySameNode,
			Created:     created,
			CreatedMS:   created * 1000,
			FinishedAt:  finished,
			Deadline:    created + 3600,
		}
		if err := db.Table(models.CronyJobHealTableName).Create(heal).Error; err != nil {
			t.Fatalf("写 job_heal 夹具失败: %v", err)
		}
	}

	after := DefaultMetricsService.Overview()
	getI := func(m map[string]interface{}, k string) int64 {
		v, ok := m[k].(int64)
		if !ok {
			t.Fatalf("指标 %q 不是 int64：%T(%v)", k, m[k], m[k])
		}
		return v
	}
	getF := func(m map[string]interface{}, k string) float64 {
		v, ok := m[k].(float64)
		if !ok {
			t.Fatalf("指标 %q 不是 float64：%T(%v)", k, m[k], m[k])
		}
		return v
	}

	// ① 窗口过滤：窗口内 3 条 succeeded 计入，40 天前那条**不计入**。
	if got, want := getI(after, "heal_succeeded")-getI(before, "heal_succeeded"), int64(3); got != want {
		t.Fatalf("heal_succeeded 增量=%d，want %d：窗口外的行被算进来了（或窗口内的行被漏掉）", got, want)
	}
	if got, want := getI(after, "heal_escalated")-getI(before, "heal_escalated"), int64(1); got != want {
		t.Fatalf("heal_escalated 增量=%d，want %d", got, want)
	}
	// ② 分母纪律：denied/planned 不进分母，所以 decided 只 +4（3 succeeded + 1 escalated）。
	if got, want := getI(after, "heal_decided")-getI(before, "heal_decided"), int64(4); got != want {
		t.Fatalf("heal_decided 增量=%d，want %d：denied/planned 被算进了分母（策略拒绝是系统正确地没动手）", got, want)
	}
	// ③ 比率恒等式（对接口返回的分子分母复算）。
	decided := getI(after, "heal_decided")
	if decided <= 0 {
		t.Fatalf("after.heal_decided=%d，夹具没生效", decided)
	}
	wantAuto := float64(getI(after, "heal_succeeded")) / float64(decided)
	if got := getF(after, "auto_repair_rate"); math.Abs(got-wantAuto) > 1e-9 {
		t.Fatalf("auto_repair_rate=%v，want %v", got, wantAuto)
	}

	// ④ MTTR：与"用同一份窗口条件在库上直接算"的结果一致。
	// 这条断言的价值在于**接口与库是两套独立实现**，取错列/取错窗口都会露出来。
	var wantMTTR float64
	if err := db.Table(models.CronyJobHealTableName).
		Select("coalesce(avg(finished_at - created),0)").
		Where("status = ? and finished_at > 0 and created >= ?",
			string(models.HealStatusSucceeded), now-7*86400).
		Scan(&wantMTTR).Error; err != nil {
		t.Fatalf("库上计算 MTTR 失败: %v", err)
	}
	if got := getF(after, "mttr_seconds"); math.Abs(got-wantMTTR) > 1e-9 {
		t.Fatalf("mttr_seconds=%v，与库上按同窗口算出的 %v 不一致", got, wantMTTR)
	}
	// 夹具里三条成功自愈的耗时是 10/20/30 秒，因此真库上 MTTR 必然 ≥ 10 且 ≤ 全窗口最大耗时。
	// 这条兜住"avg 写成了 min/max/sum"这类错法（它们同样能过上面的对账，如果两边写错得一样）。
	if wantMTTR < 10 {
		t.Fatalf("MTTR=%v 小于夹具里最短的耗时 10s（是不是把未终结行或窗口外的行算进来了）", wantMTTR)
	}

	t.Logf("窗口内夹具 7 条（3 成功/1 转人/1 拒绝/1 计划 + 1 条窗口外成功）："+
		"succeeded +%d、decided +%d、auto_repair_rate=%v、mttr=%v",
		getI(after, "heal_succeeded")-getI(before, "heal_succeeded"),
		getI(after, "heal_decided")-getI(before, "heal_decided"),
		getF(after, "auto_repair_rate"), getF(after, "mttr_seconds"))
}
