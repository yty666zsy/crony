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
