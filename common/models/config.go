package models

import "fmt"

const (
	CronyNodeTableName   = "node"
	CronyJobTableName    = "job"
	CronyJobLogTableName = "job_log"
	CronyUserTableName   = "user"
	CronyScriptTableName = "script"
	// 阶段 2：Agent 会话与轨迹。定义在 agent.go，这里只做统一索引。
	CronyAgentSessionTable = CronyAgentSessionTableName
	CronyAgentStepTable    = CronyAgentStepTableName
)

type (
	Mysql struct {
		Path         string `mapstructure:"path" json:"path" yaml:"path" ini:"path"`
		Port         string `mapstructure:"port" json:"port" yaml:"port" ini:"port"`
		Config       string `mapstructure:"config" json:"config" yaml:"config" ini:"config"`
		Dbname       string `mapstructure:"db-name" json:"dbname" yaml:"db-name" ini:"db-name"`
		Username     string `mapstructure:"username" json:"username" yaml:"username" ini:"username"`
		Password     string `mapstructure:"password" json:"password" yaml:"password" ini:"password"`
		MaxIdleConns int    `mapstructure:"max-idle-conns" json:"maxIdleConns" yaml:"max-idle-conns" ini:"max-idle-conns"`
		MaxOpenConns int    `mapstructure:"max-open-conns" json:"maxOpenConns" yaml:"max-open-conns" ini:"max-open-conns"`
		LogMode      string `mapstructure:"log-mode" json:"logMode" yaml:"log-mode" ini:"log-mode"`
		LogZap       bool   `mapstructure:"log-zap" json:"logZap" yaml:"log-zap" ini:"log-zap"`
	}
	Email struct {
		Port     int      `mapstructure:"port" json:"port" yaml:"port" ini:"port"`
		From     string   `mapstructure:"from" json:"from" yaml:"from" ini:"from"`
		Host     string   `mapstructure:"host" json:"host" yaml:"host" ini:"host"`
		IsSSL    bool     `mapstructure:"is-ssl" json:"isSSL" yaml:"is-ssl" ini:"is-ssl"`
		Secret   string   `mapstructure:"secret" json:"secret" yaml:"secret" ini:"secret"`
		Nickname string   `mapstructure:"nickname" json:"nickname" yaml:"nickname" ini:"nickname"`
		To       []string `mapstructure:"to" json:"to" yaml:"to" ini:"to"`
	}
	WebHook struct {
		Kind string `mapstructure:"kind" json:"kind" yaml:"kind" ini:"kind"`
		Url  string `mapstructure:"url" json:"url" yaml:"url" ini:"kind"`
	}
	Etcd struct {
		Endpoints   []string `mapstructure:"endpoints" json:"endpoints" yaml:"endpoints" ini:"endpoints"`
		Username    string   `mapstructure:"username" json:"username" yaml:"username" ini:"username"`
		Password    string   `mapstructure:"password" json:"password" yaml:"password" ini:"password"`
		DialTimeout int64    `mapstructure:"dial-timeout" json:"dial-timeout" yaml:"dial-timeout" ini:"dial-timeout"`
		ReqTimeout  int64    `mapstructure:"req-timeout" json:"req-timeout" yaml:"req-timeout" ini:"req-timeout"`
	}
	System struct {
		Env                string `mapstructure:"env" json:"env" yaml:"env" ini:"env"`
		Addr               int    `mapstructure:"addr" json:"addr" yaml:"addr" ini:"addr"`
		NodeTtl            int64  `mapstructure:"node-ttl" json:"node-ttl" yaml:"node-ttl" ini:"node-ttl"`
		JobProcTtl         int64  `mapstructure:"job-proc-ttl" json:"job-proc-ttl" yaml:"job-proc-ttl" ini:"job-proc-ttl"`
		Version            string `mapstructure:"version" json:"version" yaml:"version" ini:"version"`
		LogCleanPeriod     int64  `mapstructure:"log-clean-period" json:"log-clean-period" yaml:"log-clean-period" ini:"log-clean-period"`
		LogCleanExpiration int64  `mapstructure:"log-clean-expiration" json:"log-clean-expiration" yaml:"log-clean-expiration" ini:"log-clean-expiration"`
		CmdAutoAllocation  bool   `mapstructure:"cmd-auto-allocation" json:"cmd-auto-allocation" yaml:"cmd-auto-allocation" ini:"cmd-auto-allocation"`
	}
	Log struct {
		Level         string `mapstructure:"level" json:"level" yaml:"level" ini:"level"`
		Format        string `mapstructure:"format" json:"format" yaml:"format" ini:"format"`
		Prefix        string `mapstructure:"prefix" json:"prefix" yaml:"prefix" ini:"prefix"`
		Director      string `mapstructure:"director" json:"director"  yaml:"director" ini:"director"`
		ShowLine      bool   `mapstructure:"show-line" json:"showLine" yaml:"showLine" ini:"showLine"`
		EncodeLevel   string `mapstructure:"encode-level" json:"encodeLevel" yaml:"encode-level" ini:"encode-level"`
		StacktraceKey string `mapstructure:"stacktrace-key" json:"stacktraceKey" yaml:"stacktrace-key" ini:"stacktrace-key"`
		LogInConsole  bool   `mapstructure:"log-in-console" json:"logInConsole" yaml:"log-in-console" ini:"log-in-console"`
	}
	Auth struct {
		// JwtSecret JWT 签名密钥。生产环境必须显式配置（>= 32 字节随机串），
		// 可通过环境变量 CRONY_AUTH_JWT_SECRET 注入，禁止写死在代码里。
		JwtSecret string `mapstructure:"jwt-secret" json:"jwtSecret" yaml:"jwt-secret" ini:"jwt-secret"`
		// TokenExpireHours 登录令牌有效期（小时），默认 168（7 天）。
		TokenExpireHours int64 `mapstructure:"token-expire-hours" json:"tokenExpireHours" yaml:"token-expire-hours" ini:"token-expire-hours"`
		// BootstrapAdminUser 首次启动（用户表为空）时创建的初始管理员用户名。
		BootstrapAdminUser string `mapstructure:"bootstrap-admin-user" json:"bootstrapAdminUser" yaml:"bootstrap-admin-user" ini:"bootstrap-admin-user"`
		// BootstrapAdminPassword 首次启动时初始管理员口令。
		// 未配置则不会创建任何账号，改为允许"首个管理员引导注册"（见 handler/user.go Register）。
		// 该字段只应从环境变量注入，不要写进配置文件。
		BootstrapAdminPassword string `mapstructure:"bootstrap-admin-password" json:"-" yaml:"bootstrap-admin-password" ini:"bootstrap-admin-password"`
		// BootstrapAdminEmail 初始管理员邮箱（可选，用于失败告警通知）。
		BootstrapAdminEmail string `mapstructure:"bootstrap-admin-email" json:"bootstrapAdminEmail" yaml:"bootstrap-admin-email" ini:"bootstrap-admin-email"`
		// AllowPublicRegister 是否允许已存在用户后仍开放公开注册。
		// 默认 false：注册接口在非"首个管理员引导"场景下必须由管理员令牌调用。
		AllowPublicRegister bool `mapstructure:"allow-public-register" json:"allowPublicRegister" yaml:"allow-public-register" ini:"allow-public-register"`
	}
	LLM struct {
		// Enabled 是 Agent 能力的总开关。关掉后所有 Agent 接口返回 1011，不做任何模型调用。
		Enabled bool `mapstructure:"enabled" json:"enabled" yaml:"enabled" ini:"enabled"`
		// Provider 取值：openai（OpenAI 兼容 HTTP）/ offline（确定性离线 provider，用于演示与测试）；
		// 留空表示未配置模型。
		Provider string `mapstructure:"provider" json:"provider" yaml:"provider" ini:"provider"`
		// BaseURL OpenAI 兼容服务地址（例如 https://api.openai.com/v1 或自建网关）。
		BaseURL string `mapstructure:"base-url" json:"baseUrl" yaml:"base-url" ini:"base-url"`
		// APIKey 只能从环境变量注入（CRONY_LLM_API_KEY），不要写进配置文件。
		APIKey string `mapstructure:"api-key" json:"-" yaml:"api-key" ini:"api-key"`
		// Model 默认模型名（会被任务/场景覆盖时以显式配置为准）。
		Model string `mapstructure:"model" json:"model" yaml:"model" ini:"model"`
		// Temperature 采样温度；0 表示不覆盖（用服务端默认）。
		Temperature float64 `mapstructure:"temperature" json:"temperature" yaml:"temperature" ini:"temperature"`
		// MaxTokens 单次回复上限。
		MaxTokens int `mapstructure:"max-tokens" json:"maxTokens" yaml:"max-tokens" ini:"max-tokens"`
		// TimeoutSeconds 单次模型调用超时（默认 60s）。
		TimeoutSeconds int `mapstructure:"timeout-seconds" json:"timeoutSeconds" yaml:"timeout-seconds" ini:"timeout-seconds"`
		// MaxCallsPerSession 单次会话的模型调用上限（默认 20），防止修复轮/工具轮失控烧钱。
		MaxCallsPerSession int `mapstructure:"max-calls-per-session" json:"maxCallsPerSession" yaml:"max-calls-per-session" ini:"max-calls-per-session"`
		// MaxTokensPerSession 单次会话的 token 预算（默认 200000）。
		MaxTokensPerSession int `mapstructure:"max-tokens-per-session" json:"maxTokensPerSession" yaml:"max-tokens-per-session" ini:"max-tokens-per-session"`
		// MaxDurationSeconds 单次会话的墙钟预算（默认 1800s）。
		MaxDurationSeconds int `mapstructure:"max-duration-seconds" json:"maxDurationSeconds" yaml:"max-duration-seconds" ini:"max-duration-seconds"`
		// MaxSteps Agent 循环步数上限（默认 6）。
		MaxSteps int `mapstructure:"max-steps" json:"maxSteps" yaml:"max-steps" ini:"max-steps"`
		// MaxToolCalls 单次会话工具调用上限（默认 8）。
		MaxToolCalls int `mapstructure:"max-tool-calls" json:"maxToolCalls" yaml:"max-tool-calls" ini:"max-tool-calls"`
		// OfflineToolCall / OfflineResponse 仅 offline provider 使用：
		// 让"没有模型凭据"的机器也能确定性地跑通整条链路（演示与端到端测试）。
		// OfflineToolCall 形如 {"name":"log_search","arguments":{"keyword":"error"}}；
		// OfflineResponse 是最终结构化结论的 JSON 字符串。
		// 用了这两个字段时，轨迹里的 provider 一定显示 offline —— 不允许把它伪装成真实模型。
		OfflineToolCall string `mapstructure:"offline-tool-call" json:"offlineToolCall" yaml:"offline-tool-call" ini:"offline-tool-call"`
		OfflineResponse string `mapstructure:"offline-response" json:"offlineResponse" yaml:"offline-response" ini:"offline-response"`
	}
	Config struct {
		WebHook WebHook `mapstructure:"webhook" json:"webhook" yaml:"webhook" ini:"webhook"`
		Log     Log     `mapstructure:"log" json:"log" yaml:"log" ini:"log"`
		Email   Email   `mapstructure:"email" json:"email" yaml:"email" ini:"email"`
		System  System  `mapstructure:"system" json:"system" yaml:"system" ini:"system"`
		Mysql   Mysql   `mapstructure:"mysql" json:"mysql" yaml:"mysql" ini:"mysql"`
		Etcd    Etcd    `mapstructure:"etcd" json:"etcd" yaml:"etcd" ini:"etcd"`
		Auth    Auth    `mapstructure:"auth" json:"auth" yaml:"auth" ini:"auth"`
		// LLM 大模型网关（阶段 2 新增）。空配置 = 未配置模型，
		// 所有 Agent 能力必须返回明确的"未配置"错误，而不是静默降级成假结论。
		LLM LLM `mapstructure:"llm" json:"llm" yaml:"llm" ini:"llm"`
	}
)

func (m *Mysql) Dsn() string {
	return m.Username + ":" + m.Password + "@tcp(" + m.Path + ":" + m.Port + ")/" + m.Dbname + "?" + m.Config
}

func (m *Mysql) EmptyDsn() string {
	if m.Path == "" {
		m.Path = "127.0.0.1"
	}
	if m.Port == "" {
		m.Port = "3306"
	}
	return fmt.Sprintf("%s:%s@tcp(%s:%s)/", m.Username, m.Password, m.Path, m.Port)
}
