//go:build integration

package models

import (
	"fmt"
	"testing"
	"time"

	"github.com/tmnhs/crony/common/pkg/dbclient"
)

// Node.Upsert 的幂等性**必须用真库验证**：uuid 在 node 表上不是唯一键
// （只有 index:idx_node_uuid），内存替身证明不了"同一 uuid 二次登记不会多出一行"。
//
// 运行方式（不会自动跑）：
//
//	$env:CRONY_TEST_MYSQL_DSN='root:@tcp(127.0.0.1:13306)/crony_live?charset=utf8mb4&parseTime=True&loc=Local'
//	go test -tags integration ./common/models/ -run TestMySQLNodeUpsert -count=1 -v
//
// 用例只操作自己造的 uuid，跑完删除。
func TestMySQLNodeUpsertIsIdempotent(t *testing.T) {
	requireMySQL(t)
	conn := dbclient.GetMysqlDB()
	uuid := fmt.Sprintf("acc-upsert-%d", time.Now().UnixNano())
	t.Cleanup(func() { conn.Exec("delete from "+CronyNodeTableName+" where uuid = ?", uuid) })

	first := &Node{UUID: uuid, PID: "1", IP: "127.0.0.1", Hostname: "n1", Version: "v1", Status: NodeConnSuccess, UpTime: 100}
	id1, err := first.Upsert()
	if err != nil {
		t.Fatalf("首次 Upsert 失败: %v", err)
	}

	second := &Node{UUID: uuid, PID: "2", IP: "127.0.0.2", Hostname: "n1", Version: "v2", Status: NodeConnSuccess, UpTime: 200}
	id2, err := second.Upsert()
	if err != nil {
		t.Fatalf("二次 Upsert 失败: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("同一 uuid 二次登记应复用同一行，实际 id %d -> %d（节点重启产生了重复行）", id1, id2)
	}

	var cnt int64
	if err := conn.Table(CronyNodeTableName).Where("uuid = ?", uuid).Count(&cnt).Error; err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("同一 uuid 应只有一行，实际 %d 行", cnt)
	}

	var got Node
	if err := conn.Table(CronyNodeTableName).Where("uuid = ?", uuid).First(&got).Error; err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if got.PID != "2" || got.IP != "127.0.0.2" || got.Version != "v2" || got.UpTime != 200 {
		t.Fatalf("进程自身事实字段未被刷新: %+v", got)
	}

	// 运维配置字段（Labels / Capacity）是人在库里配的属性，节点不知道它们，
	// 不能被节点启动覆盖 —— 否则每次重启都会把标签/容量清空，调度亲和随之失效。
	if err := conn.Exec("update "+CronyNodeTableName+" set labels = ?, capacity = ? where uuid = ?",
		`{"env":"prod"}`, 8, uuid).Error; err != nil {
		t.Fatalf("预置标签失败: %v", err)
	}
	third := &Node{UUID: uuid, PID: "3", Status: NodeConnSuccess, UpTime: 300}
	if _, err := third.Upsert(); err != nil {
		t.Fatalf("三次 Upsert 失败: %v", err)
	}
	got = Node{}
	if err := conn.Table(CronyNodeTableName).Where("uuid = ?", uuid).First(&got).Error; err != nil {
		t.Fatalf("再读回失败: %v", err)
	}
	if got.Labels != `{"env":"prod"}` || got.Capacity != 8 {
		t.Fatalf("Upsert 不得覆盖运维配置的 labels/capacity: %+v", got)
	}
}
