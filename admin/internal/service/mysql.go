package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pkg/errors"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/crypt"
	"github.com/tmnhs/crony/common/pkg/eval"
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
	if err := autoMigrate(db); err != nil {
		return errors.Wrap(err, "register table failed")
	}
	logger.GetLogger().Info("register table success")

	if err := bootstrapAdmin(db); err != nil {
		return err
	}
	if err := bootstrapEvalSeed(db); err != nil {
		return err
	}
	return nil
}

// autoMigrate 建表，并容忍「admin 与 node 并发建表」的竞态。
//
// 为什么要重试：admin 与 node 启动时都会调用 RegisterTables。二者被同一条
// `docker compose up` 同时拉起（本地开发同时开两个终端同理）时，GORM 会并发执行
// CREATE TABLE，较慢的一方拿到 MySQL 1050（Table already exists）/1061（Duplicate key name）。
//
// 后果远不止"少建一张表"：RegisterTables 会提前 return，**初始管理员引导被整段跳过**，
// 于是"首次启动后用 .env 里的账号登录"必然失败，返回的却是"用户名或密码不正确"——
// 2026-10-14 在本机 docker compose 实测 3/3 命中，是本项目最难自查的一种启动失败。
//
// 处理方式：识别到这类冲突就重跑 AutoMigrate。此时表已存在，AutoMigrate 退化为
// 幂等的结构校验，第二次必然成功。只把"并发抢建"这一种时序变成可恢复，
// 不改变任何"表结构应该长什么样"的语义。
func autoMigrate(db *gorm.DB) error {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = db.AutoMigrate(
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
			// 阶段 5 补全（SLO 违约处置）：一次违约处置一行事实记录。
			// policy_id 索引（冷却退避按它查最近一行）= 跨进程去重的锚点。
			models.SloBreach{},
			// 阶段 5 评测：种子评测集（eval_case）与跑批结果（eval_run）。
			// case_key 是唯一键而不是自增 id——同一条断言在多次同步之间必须保持同一身份，
			// 否则历史跑批结果会挂不到同一条断言上。
			models.EvalCase{},
			models.EvalRun{},
			// 阶段 5 补全：放量档位（证据驱动的平台级上限，自动降级回 L0 的落点）。
			// env 唯一：放量按环境计，重启不丢——"重启就把 L0 忘掉"等于自动降级只活到下次部署。
			models.RolloutState{},
		)
		if err == nil {
			return nil
		}
		if !isConcurrentDDLConflict(err) {
			return err
		}
		logger.GetLogger().Warn(fmt.Sprintf(
			"[register_table] 并发建表冲突（第 %d/3 次）：%v；500ms 后重试（表已存在时 AutoMigrate 只是幂等的结构校验）",
			attempt, err))
		time.Sleep(500 * time.Millisecond)
	}
	return err
}

// isConcurrentDDLConflict 判断错误是否属于"另一个进程正在建同样的表/索引"这类可重试冲突。
//
// 先按驱动错误码判定（1050 表已存在 / 1051 表不存在 / 1061 索引名重复），
// 认不出类型时退回关键字匹配——错误经过多层包装后具体类型可能丢失，
// 但 "already exists / duplicate key name" 是 MySQL 的稳定文案。
func isConcurrentDDLConflict(err error) bool {
	if err == nil {
		return false
	}
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		switch mysqlErr.Number {
		case 1050, 1051, 1061:
			return true
		default:
			return false
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "duplicate key name")
}

// bootstrapEvalSeed 首次启动时把内置种子评测集写进 eval_case。
//
// 只在**表为空**时同步：表里已有样本说明同步过（可能还手工改过断言、停用过样本），
// 每次启动都全量覆盖会把运维的调整冲掉。要强制刷新走 POST /ops/eval/seed/sync。
func bootstrapEvalSeed(db *gorm.DB) error {
	var total int64
	if err := db.Table(models.CronyEvalCaseTableName).Count(&total).Error; err != nil {
		return errors.Wrap(err, "count eval cases failed")
	}
	if total > 0 {
		return nil
	}
	added, _, _, err := DefaultEvalBenchService.SyncEvalCases()
	if err != nil {
		return errors.Wrap(err, "seed eval cases failed")
	}
	logger.GetLogger().Info(fmt.Sprintf("[bootstrap] 已写入内置种子评测集 %d 条（版本 %s）", added, eval.SeedVersion))
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
