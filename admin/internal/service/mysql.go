package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/crypt"
	"github.com/tmnhs/crony/common/pkg/logger"
	"gorm.io/gorm"
)

// RegisterTables 建表并完成初始化。
//
// 与旧实现的区别（阶段 0 安全止血）：
//   - AutoMigrate 失败不再 os.Exit(0)：退出码 0 会被 systemd/nohup 之类的进程管理器
//     误判为"正常退出"，故障被静默掩盖；改为返回错误由调用方决定如何处理；
//   - 不再写入硬编码的 root/123456（MD5 e10adc...）默认管理员；
//     初始管理员改由配置/环境变量引导创建，口令必须是符合强度要求的 bcrypt 哈希；
//   - 未提供引导口令时不创建任何账号，改为允许"首个管理员引导注册"。
func RegisterTables(db *gorm.DB) error {
	err := db.AutoMigrate(
		models.User{},
		models.Node{},
		models.Job{},
		models.JobLog{},
		models.Script{},
		// 阶段 1（任务模型 2.0）：一次执行 + 执行步骤。
		// job_run.run_id 上有唯一索引，是幂等"至多执行一次"的最后一道闸门。
		models.JobRun{},
		models.JobRunStep{},
		// 阶段 2（Agent 内核）：会话 + 轨迹步骤。
		// agent_session.session_id 上有唯一索引，保证"同一次执行只落一次会话结论"；
		// 轨迹步骤按 (session_id, seq) 建联合索引，控制台按会话回放轨迹。
		models.AgentSession{},
		models.AgentStep{},
		// 阶段 3（观测闭环）：节点自报资源水位，供 admin 侧 node_status 工具读取。
		// node_uuid 上有唯一索引：一个节点一行"最新值"，节点重启重报不会累积垃圾行。
		models.NodeMetric{},
		// 阶段 3（观测闭环）：admin→node 只读探测请求（审计 + 结果回传）。
		// request_id 上有唯一索引：一次探测一条记录，可作为证据定位回溯
		// （证据 ref 形如 node_probe:<request_id>）。
		models.NodeProbeRequest{},
		// 阶段 4（写动作与审批）：工具调用审计（append-only）+ 审批单。
		// 审计表 session_id/decision/tool 建索引：复盘就是按这三个维度捞行；
		// 审批表 approval_id 唯一 + status/expires_at 索引：状态迁移用 CAS 更新，
		// "一个审批单至多执行一次"依赖状态机的原子迁移而不是应用层判断。
		models.AgentToolAudit{},
		models.ApprovalRequest{},
		// 阶段 4-A（Agent 记忆/经验库）：症状→动作→结果 的沉淀行。
		// dedupe_key 上有唯一索引：同一"症状+动作+目标"只留一行并累加计数，
		// 否则经验库会退化成"每次诊断多一条重复行"，召回时全是同一件事的碎片。
		models.AgentMemory{},
		// 阶段 4-C（自愈调度）：一次自愈尝试一行事实记录。
		// heal_id 唯一（幂等写），source_run_id 索引（"这次失败被怎么处置的"最常按它查），
		// status/action/failure_class 建索引：控制台按状态与动作筛选是主要用法。
		models.JobHeal{},
		// 阶段 4-D（多目标调度 / SLO 契约）：SLO 契约表。
		models.SloPolicy{},
		// 阶段 5（灰度与自治升降级）：动作×环境的自治信用表。
		models.AutonomyCredit{},
	)
	if err != nil {
		return errors.Wrap(err, "register table failed")
	}
	logger.GetLogger().Info("register table success")

	if err := bootstrapAdmin(db); err != nil {
		return err
	}
	return nil
}

// bootstrapAdmin 在用户表为空时创建初始管理员。
//
// 口令来源：auth.bootstrap-admin-password 或环境变量
// CRONY_AUTH_BOOTSTRAP_ADMIN_PASSWORD。没有配置时不创建账号，
// 也绝不回退到任何默认口令。
func bootstrapAdmin(db *gorm.DB) error {
	var total int64
	if err := db.Table(models.CronyUserTableName).Count(&total).Error; err != nil {
		return errors.Wrap(err, "count users failed")
	}
	if total > 0 {
		return nil
	}

	var username, password, email string
	if cfg := config.GetConfigModels(); cfg != nil {
		username = strings.TrimSpace(cfg.Auth.BootstrapAdminUser)
		password = cfg.Auth.BootstrapAdminPassword
		email = strings.TrimSpace(cfg.Auth.BootstrapAdminEmail)
	}
	if username == "" {
		username = "admin"
	}
	if password == "" {
		logger.GetLogger().Warn("[bootstrap] 用户表为空且未配置初始管理员口令（auth.bootstrap-admin-password / CRONY_AUTH_BOOTSTRAP_ADMIN_PASSWORD）。" +
			"不会创建任何默认账号：请调用 /register 完成首个管理员引导注册，或配置该环境变量后重启。")
		return nil
	}

	hashed, err := crypt.HashPassword(password)
	if err != nil {
		return errors.Wrapf(err, "初始管理员口令不符合强度要求（username=%s，至少 8 位且不得为常见弱口令）", username)
	}
	user := &models.User{
		UserName: username,
		Password: hashed,
		Role:     models.RoleAdmin,
		Email:    email,
		Created:  time.Now().Unix(),
	}
	if err := db.Table(models.CronyUserTableName).Create(user).Error; err != nil {
		return errors.Wrap(err, "create bootstrap admin failed")
	}
	logger.GetLogger().Info(fmt.Sprintf("[bootstrap] 已创建初始管理员 %s（口令来自环境变量/配置，未使用任何硬编码默认口令）", username))
	return nil
}
