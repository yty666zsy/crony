package models

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tmnhs/crony/common/pkg/dbclient"
)

const (
	NodeConnSuccess = 1
	NodeConnFail    = 2

	NodeSystemInfoSwitch = "alive"
)

// register to /crony/node/<node_uuid>/
type Node struct {
	ID       int    `json:"id" gorm:"column:id;primary_key;auto_increment"`
	PID      string `json:"pid" gorm:"size:16;column:pid;not null"`
	IP       string `json:"ip" gorm:"size:32;column:ip;default:''"`
	Hostname string `json:"hostname" gorm:"size:64;column:hostname;default:''"`
	UUID     string `json:"uuid" gorm:"size:128;column:uuid;not null;index:idx_node_uuid;"`
	Version  string `json:"version" gorm:"size:64;column:version;default:''"`
	Status   int    `json:"status" gorm:"size:1;column:status"`

	// Labels 能力/位置标签（JSON 对象字符串，如 {"env":"prod","zone":"az1","tool:docker":""}）。
	// 阶段 4-D（多目标调度）：调度器据此做"能力/机房亲和"，缺标签的节点不会被派发需要该能力的任务。
	// 存单列 JSON 而不是拆表：标签是节点的**属性快照**，没有独立生命周期，拆表只会多一处一致性 bug。
	Labels string `json:"labels" gorm:"type:text;column:labels"`
	// Capacity 并发上限（阶段 4-D）。<=0 表示未声明（调度按中性处理，不当作 0）。
	Capacity int `json:"capacity" gorm:"column:capacity;not null;default:0"`

	UpTime   int64 `json:"up" gorm:"column:up;not null"`
	DownTime int64 `json:"down" gorm:"column:down;default:0"`
}

// LabelMap 解析标签 JSON；解析失败或为空时返回空 map（**不报错**：
// 一个脏标签不该让整个调度链路失败，但会体现在"缺少必需标签"的拒绝原因里）。
func (n *Node) LabelMap() map[string]string {
	m := map[string]string{}
	if n == nil || strings.TrimSpace(n.Labels) == "" {
		return m
	}
	if err := json.Unmarshal([]byte(n.Labels), &m); err != nil {
		return map[string]string{}
	}
	return m
}

func (n *Node) String() string {
	return "node[" + n.UUID + "] pid[" + n.PID + "]"
}

func (n *Node) Insert() (insertId int, err error) {
	err = dbclient.GetMysqlDB().Table(CronyNodeTableName).Create(n).Error
	if err == nil {
		insertId = n.ID
	}
	return
}

// Upsert 按 uuid 就地登记：已有该 uuid 的行则**更新**，没有才插入。
//
// 为什么不能只 Insert（20 节点舰队重启实测暴露）：节点进程每次启动都会调一次登记，
// 而 uuid 在库里**不是唯一键**。于是"同一节点重启一次 → 同一 uuid 多一行"，
// 直接后果有三个：
//   - 节点页出现重复行，在线数被重复计数；
//   - FindByUUID 用 First 取的是**最旧**一行，状态更新写到旧行上，新行永远是脏的；
//   - Delete 按 uuid 删，会把重复行一起删——这本来是唯一能自愈的地方，但太粗暴。
//
// 只更新"进程自身事实"字段（pid/ip/hostname/version/status/up/down）：
// Labels / Capacity 是**运维在库里配的属性**，节点不知道它们，不能被节点启动覆盖掉。
func (n *Node) Upsert() (id int, err error) {
	var existID int
	if err = dbclient.GetMysqlDB().Table(CronyNodeTableName).
		Where("uuid = ?", n.UUID).Pluck("id", &existID).Error; err != nil {
		return
	}
	if existID > 0 {
		err = dbclient.GetMysqlDB().Table(CronyNodeTableName).Where("id = ?", existID).
			Updates(map[string]interface{}{
				"pid":      n.PID,
				"ip":       n.IP,
				"hostname": n.Hostname,
				"version":  n.Version,
				"status":   n.Status,
				"up":       n.UpTime,
				"down":     0,
			}).Error
		n.ID = existID
		return existID, err
	}
	return n.Insert()
}

func (n *Node) Update() error {
	return dbclient.GetMysqlDB().Table(CronyNodeTableName).Updates(n).Error
}

func (n *Node) Delete() error {
	return dbclient.GetMysqlDB().Exec(fmt.Sprintf("delete from %s where uuid = ?", CronyNodeTableName), n.UUID).Error
}

func (n *Node) FindByUUID() error {
	return dbclient.GetMysqlDB().Table(CronyNodeTableName).Where("uuid = ? ", n.UUID).First(n).Error
}

func (n *Node) TableName() string {
	return CronyNodeTableName
}
