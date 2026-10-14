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
	// 阶段 4：工具调用审计（append-only）与写动作审批。定义在各自文件，这里统一索引。
	CronyAgentToolAuditTable  = CronyAgentToolAuditTableName
	CronyApprovalRequestTable = CronyApprovalRequestTableName
)

// JwtIssuer JWT 签发方（`Iss`）。定义在这里是为了让"令牌实际签发的 issuer"与
// "控制台展示的 issuer"共用同一个常量，永远不会漂移。
const JwtIssuer = "tmnhs"

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
		// Tools 工具网关策略（阶段 4 新增）。
		//
		// 为什么策略必须配置化而不是硬编码：阶段 2/3 的默认策略散落在 7 处
		// `tools.DefaultPolicy()` 调用点上，各自漂移（同一个"只读 SQL 行数上限"
		// 在网关里是 100、在执行路径里是 200）。配置化之后**只有一个来源**，
		// 而且"这台部署到底放开了什么"可以从配置文件直接读出来——
		// 安全边界的可读性和它的正确性一样重要。
		Tools Tools `mapstructure:"tools" json:"tools" yaml:"tools" ini:"tools"`
		// SelfHeal 自愈调度策略（阶段 4-C 新增）。
		//
		// 为什么单独一段配置而不是塞进 tools：自愈不是"模型调工具"，
		// 它有自己的开关语义（总开关 + 任务级 opt-in + 动作白名单 + 频次闸门），
		// 和工具网关的自治级别不是一回事。混在一起会让两边都说不清"到底谁在管"。
		SelfHeal SelfHeal `mapstructure:"self-heal" json:"selfHeal" yaml:"self-heal" ini:"self-heal"`
		// Slo SLO 契约的违约处置配置（阶段 5 补全）。
		//
		// 为什么不塞进 self-heal：两者负责的是不同的事——
		// self-heal 对"某次执行失败"负责，SLO 对"一段时间内的结果"负责。
		// 混在一起会让"周期性结果违约"与"单次失败"共用一套闸门，语义混淆。
		Slo Slo `mapstructure:"slo" json:"slo" yaml:"slo" ini:"slo"`
		// K8s Kubernetes 集群接入（阶段 K1 新增）。
		//
		// 缺省 = 未接入集群：k8s 任务步骤会**明确失败**（"本节点未接入 Kubernetes"），
		// 只读的 k8s_readonly 工具干脆不注册（而不是注册一个"调用必失败"的工具，
		// 那会让模型以为"看过集群了，什么都没发现"）。
		K8s K8s `mapstructure:"k8s" json:"k8s" yaml:"k8s" ini:"k8s"`
	}
	// K8s 集群接入配置（阶段 K1）。
	//
	// 这里的每一项都是一条**边界**，不是"可选优化"：
	// 创建 Pod 等于在集群里执行任意代码，所以默认一律关闭，必须显式打开。
	K8s struct {
		// Enabled 总开关（默认 false）。关着时不做任何集群调用。
		Enabled bool `mapstructure:"enabled" json:"enabled" yaml:"enabled" ini:"enabled"`
		// Kubeconfig kubeconfig 路径（空 = in-cluster → KUBECONFIG → ~/.kube/config）。
		Kubeconfig string `mapstructure:"kubeconfig" json:"kubeconfig" yaml:"kubeconfig" ini:"kubeconfig"`
		// Context kubeconfig 里的 context 名（空 = current-context）。
		Context string `mapstructure:"context" json:"context" yaml:"context" ini:"context"`
		// Namespace 默认命名空间（默认 default）。
		Namespace string `mapstructure:"namespace" json:"namespace" yaml:"namespace" ini:"namespace"`
		// AllowNamespaces 额外允许的命名空间（默认空 = 只允许 namespace 那一个）。
		AllowNamespaces []string `mapstructure:"allow-namespaces" json:"allowNamespaces" yaml:"allow-namespaces" ini:"allow-namespaces"`
		// AllowImages 允许的容器镜像白名单，支持 `busybox*` 前缀通配。
		//
		// 默认空 = **不允许任何镜像**（这是刻意的默认关闭）。写 `["*"]` 表示接受任意镜像。
		AllowImages []string `mapstructure:"allow-images" json:"allowImages" yaml:"allow-images" ini:"allow-images"`
		// DefaultCPU / DefaultMemory 容器 requests/limits（默认空 = 不设置）。
		DefaultCPU    string `mapstructure:"default-cpu" json:"defaultCpu" yaml:"default-cpu" ini:"default-cpu"`
		DefaultMemory string `mapstructure:"default-memory" json:"defaultMemory" yaml:"default-memory" ini:"default-memory"`
		// ServiceAccountName Pod 使用的服务账号（默认空 = default）。
		ServiceAccountName string `mapstructure:"service-account-name" json:"serviceAccountName" yaml:"service-account-name" ini:"service-account-name"`
		// TTLSecondsAfterFinished Job 结束后保留多久自动清理（默认 600s）。
		TTLSecondsAfterFinished int32 `mapstructure:"ttl-seconds-after-finished" json:"ttlSecondsAfterFinished" yaml:"ttl-seconds-after-finished" ini:"ttl-seconds-after-finished"`
		// PollIntervalSeconds 等待 Job 结束的轮询间隔（默认 2s）。
		PollIntervalSeconds int `mapstructure:"poll-interval-seconds" json:"pollIntervalSeconds" yaml:"poll-interval-seconds" ini:"poll-interval-seconds"`
		// AllowPrivilegeEscalation 允许容器提权（默认 false）。
		AllowPrivilegeEscalation bool `mapstructure:"allow-privilege-escalation" json:"allowPrivilegeEscalation" yaml:"allow-privilege-escalation" ini:"allow-privilege-escalation"`
	}
	// SelfHeal 自愈调度配置。所有默认值都是关闭/收紧的一档（见 selfheal.DefaultPolicy）。
	SelfHeal struct {
		// Enabled 总开关（默认 false）。关着时扫描只做分类与留痕，不做任何动作。
		Enabled bool `mapstructure:"enabled" json:"enabled" yaml:"enabled" ini:"enabled"`
		// IntervalSeconds 扫描间隔（默认 60s）。
		IntervalSeconds int64 `mapstructure:"interval-seconds" json:"intervalSeconds" yaml:"interval-seconds" ini:"interval-seconds"`
		// MaxAttemptsPerJobWindow 同一任务在窗口内的自愈尝试上限（默认 3）。
		MaxAttemptsPerJobWindow int `mapstructure:"max-attempts-per-job-window" json:"maxAttemptsPerJobWindow" yaml:"max-attempts-per-job-window" ini:"max-attempts-per-job-window"`
		// WindowSeconds 尝试次数统计窗口（默认 1800s）。
		WindowSeconds int64 `mapstructure:"window-seconds" json:"windowSeconds" yaml:"window-seconds" ini:"window-seconds"`
		// CooldownSeconds 同一任务两次自愈的最小间隔（默认 300s）。
		CooldownSeconds int64 `mapstructure:"cooldown-seconds" json:"cooldownSeconds" yaml:"cooldown-seconds" ini:"cooldown-seconds"`
		// ExtendTimeoutFactor 超时自愈的放大倍数（默认 2）。
		ExtendTimeoutFactor float64 `mapstructure:"extend-timeout-factor" json:"extendTimeoutFactor" yaml:"extend-timeout-factor" ini:"extend-timeout-factor"`
		// MaxExtendTimeoutSeconds 超时上限的绝对天花板（默认 3600s）。
		MaxExtendTimeoutSeconds int64 `mapstructure:"max-extend-timeout-seconds" json:"maxExtendTimeoutSeconds" yaml:"max-extend-timeout-seconds" ini:"max-extend-timeout-seconds"`
		// MaxConcurrent 同时待观测的自愈上限（默认 2）。
		MaxConcurrent int `mapstructure:"max-concurrent" json:"maxConcurrent" yaml:"max-concurrent" ini:"max-concurrent"`
		// ObserveWindowSeconds 自愈后等待新执行的观测窗口（默认 900s）。
		ObserveWindowSeconds int64 `mapstructure:"observe-window-seconds" json:"observeWindowSeconds" yaml:"observe-window-seconds" ini:"observe-window-seconds"`
		// ScanLimit 单轮最多处置多少条失败执行（默认 50）。
		ScanLimit int `mapstructure:"scan-limit" json:"scanLimit" yaml:"scan-limit" ini:"scan-limit"`
		// LookbackSeconds 只扫描该时间窗内结束的失败执行（默认 1800s）。
		LookbackSeconds int64 `mapstructure:"lookback-seconds" json:"lookbackSeconds" yaml:"lookback-seconds" ini:"lookback-seconds"`
	}
	// Slo SLO 违约处置配置（阶段 5 补全）。缺省 = 关闭动作、冷却 30 分钟。
	//
	// 默认关闭 = 与接线前完全一致（worker 仍只评估与打日志），零回归。
	Slo struct {
		// BreachActionEnabled 周期评估发现违约后是否**执行**契约里的 breach_action。
		// 默认 false：worker 只评估 + 打日志，不做任何外部动作。
		BreachActionEnabled bool `mapstructure:"breach-action-enabled" json:"breachActionEnabled" yaml:"breach-action-enabled" ini:"breach-action-enabled"`
		// CooldownSeconds 同一契约两次动作之间的最小间隔，默认 1800s。
		// 它同时充当**跨进程退避**：worker 与 admin 都跑时，靠最近一条 slo_breach 的时间去重，
		// 而不是各自内存里的 map（重启即丢）。
		CooldownSeconds int64 `mapstructure:"cooldown-seconds" json:"cooldownSeconds" yaml:"cooldown-seconds" ini:"cooldown-seconds"`
	}
	// Tools 工具网关策略（阶段 4）。所有字段都有保守默认值；
	// 缺失/留空 = 用默认值，且默认值一律是"关着"的那一档。
	Tools struct {
		// AllowWrite 是否允许注册可逆写工具（默认 false）。
		AllowWrite bool `mapstructure:"allow-write" json:"allowWrite" yaml:"allow-write" ini:"allow-write"`
		// AllowDestructive 是否允许注册不可逆/高爆工具（默认 false；即使开启也永远需要审批）。
		AllowDestructive bool `mapstructure:"allow-destructive" json:"allowDestructive" yaml:"allow-destructive" ini:"allow-destructive"`
		// Autonomy 自治级别：L0 只建议 / L1 一律审批（默认） / L2 白名单自动 / L3 全自动。
		Autonomy string `mapstructure:"autonomy" json:"autonomy" yaml:"autonomy" ini:"autonomy"`
		// AutoApprove L2/L3 的免审批白名单，条目为 `工具名` 或 `工具名@目标`（默认空）。
		AutoApprove []string `mapstructure:"auto-approve" json:"autoApprove" yaml:"auto-approve" ini:"auto-approve"`
		// ApprovalTTLSeconds 审批单有效期（默认 900s = 15 分钟）。
		ApprovalTTLSeconds int `mapstructure:"approval-ttl-seconds" json:"approvalTtlSeconds" yaml:"approval-ttl-seconds" ini:"approval-ttl-seconds"`
		// MaxCallsPerSession 单会话工具调用上限（默认 12）。
		MaxCallsPerSession int `mapstructure:"max-calls-per-session" json:"maxCallsPerSession" yaml:"max-calls-per-session" ini:"max-calls-per-session"`
		// MaxCallsPerTool 单会话同一工具上限（默认 4）。
		MaxCallsPerTool int `mapstructure:"max-calls-per-tool" json:"maxCallsPerTool" yaml:"max-calls-per-tool" ini:"max-calls-per-tool"`
		// MaxCallsPerMinute 进程级速率上限（默认 60）。
		MaxCallsPerMinute int `mapstructure:"max-calls-per-minute" json:"maxCallsPerMinute" yaml:"max-calls-per-minute" ini:"max-calls-per-minute"`
		// MaxConcurrentCalls 进程级并发上限（默认 4）。
		MaxConcurrentCalls int `mapstructure:"max-concurrent-calls" json:"maxConcurrentCalls" yaml:"max-concurrent-calls" ini:"max-concurrent-calls"`
		// MaxWriteActionsPerSession 单会话写动作上限（默认 2）。
		MaxWriteActionsPerSession int `mapstructure:"max-write-actions-per-session" json:"maxWriteActionsPerSession" yaml:"max-write-actions-per-session" ini:"max-write-actions-per-session"`
		// MaxAffectedHostsPerSession 单会话可影响的去重主机数（默认 1）。
		MaxAffectedHostsPerSession int `mapstructure:"max-affected-hosts-per-session" json:"maxAffectedHostsPerSession" yaml:"max-affected-hosts-per-session" ini:"max-affected-hosts-per-session"`
		// DefaultTimeoutSeconds 单次工具默认超时（默认 15s）。
		DefaultTimeoutSeconds int `mapstructure:"default-timeout-seconds" json:"defaultTimeoutSeconds" yaml:"default-timeout-seconds" ini:"default-timeout-seconds"`
		// MaxOutputBytes 单次工具输出上限（默认 65536）。
		MaxOutputBytes int `mapstructure:"max-output-bytes" json:"maxOutputBytes" yaml:"max-output-bytes" ini:"max-output-bytes"`
		// AllowHosts http_get 允许的 host（默认空 = 完全禁止外部请求）。
		AllowHosts []string `mapstructure:"allow-hosts" json:"allowHosts" yaml:"allow-hosts" ini:"allow-hosts"`
		// ShellReadPaths tail 等可读路径前缀白名单（默认 /var/log、/var/log/crony）。
		ShellReadPaths []string `mapstructure:"shell-read-paths" json:"shellReadPaths" yaml:"shell-read-paths" ini:"shell-read-paths"`
		// SQLMaxRows 只读 SQL 行数上限（默认 100）。
		SQLMaxRows int `mapstructure:"sql-max-rows" json:"sqlMaxRows" yaml:"sql-max-rows" ini:"sql-max-rows"`
		// Deny 显式禁止的工具（优先级高于一切开关）。
		Deny []string `mapstructure:"deny" json:"deny" yaml:"deny" ini:"deny"`
		// Allow 工具白名单（非空时只允许这些）。
		Allow []string `mapstructure:"allow" json:"allow" yaml:"allow" ini:"allow"`
		// AutonomyCredit 阶段 5：是否用「自治信用」驱动写动作的实际生效级别（默认 false）。
		//
		// 开启后：写动作的实际生效级别 = min(Autonomy 上限, 该动作的信用级别)。
		// 信用只能**收紧**不能越权；没有历史的动作沿用 Autonomy。
		// 默认 false = 与阶段 4 行为完全一致（零回归）。
		AutonomyCredit bool `mapstructure:"autonomy-credit" json:"autonomyCredit" yaml:"autonomy-credit" ini:"autonomy-credit"`
		// AutonomyEnv 自治信用 scope 的环境段（默认 "default"），scope 形如 `job_retry@<env>`。
		// 为什么要有环境维度："重跑在测试环境很安全"不等于"在生产很安全"。
		AutonomyEnv string `mapstructure:"autonomy-env" json:"autonomyEnv" yaml:"autonomy-env" ini:"autonomy-env"`
		// RolloutGate 阶段 5 补全：是否把「放量档位」也接进写动作的放行判定（默认 false）。
		//
		// 开启后：实际生效级别 = min(Autonomy 上限, 信用级别, 放量档位)。
		// 放量档位只能靠**新鲜且干净的证据**（评测 + SLO）加宽，坏信号自动收回 L0；
		// 没有放量记录时不表态（沿用配置），所以开启本开关本身不改变行为（零回归）。
		RolloutGate bool `mapstructure:"rollout-gate" json:"rolloutGate" yaml:"rollout-gate" ini:"rollout-gate"`
		// RolloutAutoEval 阶段 5 补全：是否让进程内的定时循环**自动**调用一次放量评估（默认 false）。
		//
		// 在此之前，"坏信号自动收回 L0"的判定虽是全自动的，却只能靠人/脚本调
		// `POST /ops/rollout/evaluate` 驱动；本开关把它挂进与自愈/SLO 同一套常驻循环。
		//
		// 一条刻意的不变式：循环**只收回、不新造**。没有 rollout_state 记录的环境，
		// 循环不会凭空写一条 L0 —— 否则"打开开关"就不再是零回归，而是一次全平台行为突变
		// （本机库内长期有 SLO 违约，一开就会把每个环境都压到 L0）。
		RolloutAutoEval bool `mapstructure:"rollout-auto-eval" json:"rolloutAutoEval" yaml:"rollout-auto-eval" ini:"rollout-auto-eval"`
		// RolloutAutoEvalSeconds 自动降级循环的节拍（秒，默认 300；<=0 用默认值）。
		RolloutAutoEvalSeconds int `mapstructure:"rollout-auto-eval-seconds" json:"rolloutAutoEvalSeconds" yaml:"rollout-auto-eval-seconds" ini:"rollout-auto-eval-seconds"`
		// RolloutAutoEvalCooldownSeconds 同一环境两次自动评估之间的最小间隔（秒，默认 1800）。
		// 它同时充当**跨进程退避**：admin 与 worker 都跑时，靠 rollout_state.tightened_at
		// 去重，而不是各自内存里的时间戳（重启即丢）。
		RolloutAutoEvalCooldownSeconds int64 `mapstructure:"rollout-auto-eval-cooldown-seconds" json:"rolloutAutoEvalCooldownSeconds" yaml:"rollout-auto-eval-cooldown-seconds" ini:"rollout-auto-eval-cooldown-seconds"`
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
