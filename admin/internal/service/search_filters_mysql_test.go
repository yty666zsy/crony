//go:build integration

// 控制台检索契约的**真库**集成测试（阶段 5 收尾修的三个"筛选静默失效"缺陷）。
//
// 为什么必须是真库：这三条都不是纯函数问题，而是 SQL where 的取舍，
// 且失效方式完全一样——**静默返回全部**，单测里手写候选集永远看不出来：
//   - F7 节点检索：hostname 字段在请求结构体里根本不存在 → 输入任何内容都返回全部；
//     同时 ip 当时用的是精确匹配，"只记得一半"同样搜不到；
//   - F8 任务检索：未分配状态的枚举值就是 0，而实现用 `status > 0` 判断"是否筛选"，
//     导致"筛选未分配"这个条件永远不生效；
//   - F6 经验检索：前端传 action_kind，后端请求结构体/MemoryQuery 都没有这个字段，
//     筛选被整个忽略（按钮点了等于没点）。
//
// 运行方式同其它集成测试：CRONY_TEST_MYSQL_DSN 指向临时库（13306），未设置即跳过。

package service

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"gorm.io/gorm"
)

// itFilterDB 连真库并保证节点/任务/经验三张表存在。
func itFilterDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := strings.TrimSpace(os.Getenv("CRONY_TEST_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("未设置 CRONY_TEST_MYSQL_DSN，跳过真实 MySQL 集成测试")
	}
	db, err := dbclient.Init(dsn, "silent", 1, 5)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	if db == nil {
		t.Fatal("dbclient.Init 返回了空连接")
	}
	if err := db.AutoMigrate(&models.Node{}, &models.Job{}, &models.AgentMemory{}); err != nil {
		t.Fatalf("迁移失败（测试库需要 CREATE 权限）: %v", err)
	}
	return db
}

// TestIntegrationNodeSearchHostnameAndIPFilter 验证节点检索真的按 hostname/IP 过滤。
func TestIntegrationNodeSearchHostnameAndIPFilter(t *testing.T) {
	db := itFilterDB(t)
	tag := itMemTag("nodef")

	hit := &models.Node{
		PID: "1", IP: "10.77.241.31", Hostname: tag + "-alpha", UUID: tag + "-n1",
		Version: "v-it", Status: models.NodeConnSuccess, UpTime: time.Now().Unix(),
	}
	miss := &models.Node{
		PID: "2", IP: "10.88.152.42", Hostname: "unrelated-beta-" + tag, UUID: tag + "-n2",
		Version: "v-it", Status: models.NodeConnSuccess, UpTime: time.Now().Unix(),
	}
	for _, n := range []*models.Node{hit, miss} {
		if err := db.Table(models.CronyNodeTableName).Create(n).Error; err != nil {
			t.Fatalf("写节点失败: %v", err)
		}
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM "+models.CronyNodeTableName+" WHERE uuid LIKE ?", tag+"%")
	})

	svc := NewNodeWatcherService()

	// 1) hostname 模糊匹配：用 tag 片段 -> 只能命中 hit（此前该字段没接，会返回全部节点）。
	got, total, err := svc.Search(&request.ReqNodeSearch{Hostname: tag + "-alph"})
	if err != nil {
		t.Fatalf("按 hostname 检索失败: %v", err)
	}
	if total != 1 || len(got) != 1 || got[0].UUID != hit.UUID {
		t.Fatalf("hostname 模糊匹配应只命中 1 台（%s），实际 total=%d len=%d", hit.UUID, total, len(got))
	}

	// 2) 不存在的 hostname -> 必须为空（"筛选生效"的反证）。
	_, noneTotal, err := svc.Search(&request.ReqNodeSearch{Hostname: "zzz-not-exist-" + tag})
	if err != nil {
		t.Fatalf("按不存在的 hostname 检索失败: %v", err)
	}
	if noneTotal != 0 {
		t.Fatalf("不存在的 hostname 必须返回 0，实际 %d（说明筛选没生效，仍然返回全部）", noneTotal)
	}

	// 3) IP 模糊匹配：只给一段也能搜到（此前是精确匹配，部分 IP 搜不到）。
	got, ipTotal, err := svc.Search(&request.ReqNodeSearch{IP: "10.77.241.31"})
	if err != nil {
		t.Fatalf("按 IP 检索失败: %v", err)
	}
	if ipTotal < 1 || len(got) < 1 {
		t.Fatalf("IP 模糊匹配应至少命中本轮节点，实际 total=%d", ipTotal)
	}
	seenHit := false
	for _, n := range got {
		if !strings.Contains(n.IP, "10.77.241.31") {
			t.Fatalf("IP 过滤返回了不含该片段的节点: %s", n.IP)
		}
		if n.UUID == hit.UUID {
			seenHit = true
		}
	}
	if !seenHit {
		t.Fatalf("IP 模糊匹配没命中本轮节点 %s", hit.UUID)
	}
}

// TestIntegrationJobSearchStatusZero 验证 status=0（未分配）是**可筛选**的，而不是"没传"。
func TestIntegrationJobSearchStatusZero(t *testing.T) {
	db := itFilterDB(t)
	tag := itMemTag("jobf")
	prefix := "itjobf-" + tag

	jobs := []*models.Job{
		{ID: 910060, Name: prefix + "-unassigned", Command: "echo a", Type: models.JobTypeCmd,
			Spec: "0 0 1 1 *", Status: models.JobStatusNotAssigned, Timeout: 60},
		{ID: 910061, Name: prefix + "-assigned", Command: "echo b", Type: models.JobTypeCmd,
			Spec: "0 0 1 1 *", Status: models.JobStatusAssigned, Timeout: 60},
	}
	for _, j := range jobs {
		if err := db.Table(models.CronyJobTableName).Create(j).Error; err != nil {
			t.Fatalf("写任务失败: %v", err)
		}
	}
	t.Cleanup(func() {
		db.Exec("DELETE FROM "+models.CronyJobTableName+" WHERE name LIKE ?", prefix+"%")
	})

	// 名字前缀把结果限定在本轮的两条上，避免受库里既有任务影响。
	_, allTotal, err := DefaultJobService.Search(&request.ReqJobSearch{Name: prefix})
	if err != nil {
		t.Fatalf("不带 status 检索失败: %v", err)
	}
	if allTotal != 2 {
		t.Fatalf("本轮应能检索到 2 条任务，实际 %d", allTotal)
	}

	zero := models.JobStatusNotAssigned // 0
	got, zeroTotal, err := DefaultJobService.Search(&request.ReqJobSearch{Name: prefix, Status: &zero})
	if err != nil {
		t.Fatalf("status=0 检索失败: %v", err)
	}
	if zeroTotal != 1 || len(got) != 1 || got[0].Status != models.JobStatusNotAssigned {
		t.Fatalf("status=0 应只命中未分配的那条，实际 total=%d len=%d（此前 0 被当成\"没传\"，会返回 2）", zeroTotal, len(got))
	}

	one := models.JobStatusAssigned // 1
	_, oneTotal, err := DefaultJobService.Search(&request.ReqJobSearch{Name: prefix, Status: &one})
	if err != nil {
		t.Fatalf("status=1 检索失败: %v", err)
	}
	if oneTotal != 1 {
		t.Fatalf("status=1 应只命中已分配的那条，实际 %d", oneTotal)
	}
}

// TestIntegrationMemorySearchActionKind 验证经验检索按 action_kind 过滤（tool / advice）。
func TestIntegrationMemorySearchActionKind(t *testing.T) {
	db := itMemDB(t)
	prefix := itMemTag("kindf")
	svc := NewMemoryService(NewGormMemoryStore())
	t.Cleanup(func() { itMemCleanup(t, db, prefix) })

	now := time.Now()
	sess := prefix + "-sess"

	toolObs := itMemObs(prefix, prefix+" 症状：只给了工具动作", "disk_df /data", "node-it-01",
		models.MemoryOutcomeSuccess, now)
	toolObs.SessionID = sess
	advObs := itMemObs(prefix+"-adv", prefix+" 症状：只给了建议", "建议人工核对日志", "node-it-02",
		models.MemoryOutcomeAdvised, now.Add(time.Second))
	advObs.SessionID = sess
	advObs.ActionKind = models.MemoryActionAdvice

	if _, created := itMemUpsert(t, db, toolObs, now); !created {
		t.Fatal("工具动作经验应为新建")
	}
	if _, created := itMemUpsert(t, db, advObs, now.Add(time.Second)); !created {
		t.Fatal("建议类经验应为新建")
	}

	_, allTotal, err := svc.Search(models.MemoryQuery{SessionID: sess, PageSize: 50})
	if err != nil {
		t.Fatalf("不带 action_kind 检索失败: %v", err)
	}
	if allTotal != 2 {
		t.Fatalf("本轮应能检索到 2 条经验，实际 %d", allTotal)
	}

	cases := []struct {
		kind string
		want string
	}{
		{models.MemoryActionTool, models.MemoryActionTool},
		{models.MemoryActionAdvice, models.MemoryActionAdvice},
	}
	for _, c := range cases {
		rows, total, err := svc.Search(models.MemoryQuery{SessionID: sess, ActionKind: c.kind, PageSize: 50})
		if err != nil {
			t.Fatalf("action_kind=%s 检索失败: %v", c.kind, err)
		}
		if total != 1 || len(rows) != 1 {
			t.Fatalf("action_kind=%s 应只命中 1 条，实际 total=%d len=%d（此前该条件被忽略，会返回 2）", c.kind, total, len(rows))
		}
		if rows[0].ActionKind != c.want {
			t.Fatalf("action_kind=%s 命中行的类型是 %s", c.kind, rows[0].ActionKind)
		}
	}
}
