package config

import (
	"fmt"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/utils"
	"os"
	"path"
	"strings"
)

const (
	ExtensionJson = ".json"
	ExtensionYaml = ".yaml"
	ExtensionInI  = ".ini"

	NameSpace = "conf"

	// EnvPrefix 环境变量前缀。例如 auth.jwt-secret 对应 CRONY_AUTH_JWT_SECRET，
	// mysql.password 对应 CRONY_MYSQL_PASSWORD。
	// 这样密钥/口令可以只从环境变量注入，不落配置文件、不进代码仓库。
	EnvPrefix = "CRONY"
)

const (
	// DefaultNodeTtl 节点注册租约的**保守默认值**（秒）。
	//
	// 为什么不是 5s（20 节点舰队实测，F6）：node-ttl 直接进 etcd 的 LeaseGrant，
	// 而**租约过期就是 FailOver 的唯一触发器**。5s 在"多节点同机部署"下偏激进——
	// 一次 GC 停顿/CPU 争抢/网络抖动就够让租约过期，把**健康节点**误判成失联、
	// 白搬一次任务（还会连带删掉它的库内行）。15s 让"抖动"和"真死"能被区分开。
	DefaultNodeTtl int64 = 15
	// MinNodeTtl 低于该值视为"过激进"：照用，但必须告警（不静默改写用户显式配置）。
	MinNodeTtl int64 = 5
)

// ResolveNodeTtl 计算节点注册租约的**有效 TTL**（秒），返回 (ttl, warning)。
//
// 口径：
//   - configured <= 0（没配 / 配错 / 解析失败得到零值）→ 用 DefaultNodeTtl。
//     **这是一条硬保护**：etcd 对 ttl<=0 会退回**最小租约（约 1s）**，
//     于是节点被反复判死、反复 FailOver——一个会让集群自己打自己的配置错误，
//     绝不能因为"配置文件漏写了一行"就发生。
//   - 0 < configured < MinNodeTtl → 照用（用户显式配的），但返回告警。
//   - 其余 → 照用，无告警。
func ResolveNodeTtl(configured int64) (ttl int64, warning string) {
	if configured <= 0 {
		return DefaultNodeTtl, fmt.Sprintf(
			"node-ttl 未配置或非法（%d）：已回退到保守默认 %ds（etcd 对 ttl<=0 会退回最小租约，节点会被反复误判失联）",
			configured, DefaultNodeTtl)
	}
	if configured < MinNodeTtl {
		return configured, fmt.Sprintf(
			"node-ttl=%ds 偏激进（建议 >= %ds）：多节点同机部署时一次抖动就可能让租约过期，把健康节点误判为失联并触发无谓 FailOver",
			configured, MinNodeTtl)
	}
	return configured, ""
}

var (
	//Automatic loading sequence of local Config
	autoLoadLocalConfigs = []string{
		ExtensionJson,
		ExtensionYaml,
		ExtensionInI,
	}
)

var _defaultConfig *models.Config

func LoadConfig(env, serverName, configFileName string) (*models.Config, error) {
	var c models.Config
	var confPath string
	dir := fmt.Sprintf("%s/%s/%s", serverName, NameSpace, env)
	for _, registerExt := range autoLoadLocalConfigs {
		confPath = path.Join(dir, configFileName+registerExt)
		if utils.Exists(confPath) {
			break
		}
	}
	fmt.Println("the path to the configuration file you are using is :", confPath)
	v := viper.New()
	v.SetConfigFile(confPath)
	ext := utils.Ext(confPath)
	v.SetConfigType(ext)
	err := v.ReadInConfig()
	if err != nil {
		panic(fmt.Errorf("Fatal error config file: %s \n", err))
	}
	v.WatchConfig()

	v.OnConfigChange(func(e fsnotify.Event) {
		fmt.Println("config file changed:", e.Name)
		applyEnvOverrides(v)
		if err := v.Unmarshal(&c); err != nil {
			fmt.Println(err)
		}
	})
	applyEnvOverrides(v)
	if err := v.Unmarshal(&c); err != nil {
		fmt.Println(err)
	}
	printConfigSummary(&c)
	_defaultConfig = &c
	return &c, nil
}

// applyEnvOverrides 让环境变量覆盖配置文件中的同名键。
//
// viper 的 AutomaticEnv 只影响 Get() 查询，不影响 Unmarshal，
// 因此这里显式遍历已注册的键、把存在的环境变量 v.Set 回配置树，
// 保证 Unmarshal 与 Get 看到同一份值。
//
// 键名映射：`.` 和 `-` 都替换为 `_`，并加前缀 CRONY_。
// 例如 auth.jwt-secret -> CRONY_AUTH_JWT_SECRET。
//
// 注意：v.AllKeys() 只返回"已注册"的键（配置文件里出现过的 + SetDefault 的），
// 所以必须先注册默认值，否则老版本配置文件里没有的 auth.* 键
// 无法通过环境变量注入。
func applyEnvOverrides(v *viper.Viper) {
	registerDefaults(v)
	replacer := strings.NewReplacer(".", "_", "-", "_")
	for _, key := range v.AllKeys() {
		envKey := EnvPrefix + "_" + strings.ToUpper(replacer.Replace(key))
		if value, ok := os.LookupEnv(envKey); ok {
			v.Set(key, value)
		}
	}
}

// registerDefaults 注册所有允许通过环境变量注入的键的默认值。
// 这些默认值同时也是"配置缺省时的安全缺省"：
// jwt-secret 默认为空 => 非生产环境生成随机会话密钥，生产环境拒绝启动。
//
// llm.* 也必须注册：APIKey 的 mapstructure 标签是 api-key，而 struct 注释写明
// "只能从环境变量注入（CRONY_LLM_API_KEY）"。但 viper 的 Unmarshal 不会为
// 未注册的键读环境变量（applyEnvOverrides 也只遍历 v.AllKeys()），
// 因此如果配置文件里没写 llm 块，CRONY_LLM_API_KEY 会被**静默丢弃**：
// 真实联调时表现为模型调用 401 "You didn't provide an API key"。
// 这里把整段 llm 配置注册进 viper，让"密钥只走环境变量"这条设计真正可用。
func registerDefaults(v *viper.Viper) {
	v.SetDefault("auth.jwt-secret", "")
	v.SetDefault("auth.token-expire-hours", 168)
	v.SetDefault("auth.bootstrap-admin-user", "admin")
	v.SetDefault("auth.bootstrap-admin-password", "")
	v.SetDefault("auth.bootstrap-admin-email", "")
	v.SetDefault("auth.allow-public-register", false)

	// Agent / 模型凭据（阶段 2）
	v.SetDefault("llm.enabled", false)
	v.SetDefault("llm.provider", "")
	v.SetDefault("llm.base-url", "")
	v.SetDefault("llm.api-key", "")
	v.SetDefault("llm.model", "")
	v.SetDefault("llm.temperature", 0)
	v.SetDefault("llm.max-tokens", 0)
	v.SetDefault("llm.timeout-seconds", 60)
	v.SetDefault("llm.max-calls-per-session", 20)
	v.SetDefault("llm.max-tokens-per-session", 200000)
	v.SetDefault("llm.max-duration-seconds", 1800)
	v.SetDefault("llm.max-steps", 6)
	v.SetDefault("llm.max-tool-calls", 8)
	v.SetDefault("llm.offline-tool-call", "")
	v.SetDefault("llm.offline-response", "")

	// k8s.* 同样显式注册：默认全是"关着"的那一档。
	// 特别是 allow-images 默认为空 = 不允许任何镜像（创建 Pod 等于在集群里跑任意代码）。
	v.SetDefault("k8s.enabled", false)
	v.SetDefault("k8s.kubeconfig", "")
	v.SetDefault("k8s.context", "")
	v.SetDefault("k8s.namespace", "default")
	v.SetDefault("k8s.allow-namespaces", []string{})
	v.SetDefault("k8s.allow-images", []string{})
	v.SetDefault("k8s.default-cpu", "")
	v.SetDefault("k8s.default-memory", "")
	v.SetDefault("k8s.service-account-name", "")
	v.SetDefault("k8s.ttl-seconds-after-finished", 600)
	v.SetDefault("k8s.poll-interval-seconds", 2)
	v.SetDefault("k8s.allow-privilege-escalation", false)

	// SLO 违约处置（阶段 5 补全）。注册默认值有两个作用：
	//  1) 配置文件里没有 slo 段时，Unmarshal 得到"关闭 + 冷却 30 分钟"的安全缺省；
	//  2) 让 CRONY_SLO_BREACH_ACTION_ENABLED / CRONY_SLO_COOLDOWN_SECONDS 可注入。
	v.SetDefault("slo.breach-action-enabled", false)
	v.SetDefault("slo.cooldown-seconds", 1800)

	// 节点注册租约 TTL（秒）。注册它的作用：
	//  1) 配置文件里没有 system 段时，租约不会退化成 etcd 的最小租约（约 1s）——
	//     那会让节点被反复判死并触发 FailOver（20 节点舰队实测，F6）；
	//  2) 让 CRONY_SYSTEM_NODE_TTL 可注入；
	//  3) 与 ResolveNodeTtl 的兜底口径一致（<=0 时回退到 DefaultNodeTtl）。
	v.SetDefault("system.node-ttl", DefaultNodeTtl)
}

// printConfigSummary 打印一份脱敏后的配置摘要。
// 原实现用 %#v 打印整个配置结构体，会把 MySQL/etcd/邮箱口令明文写进日志，
// 属于阶段 0 必须消除的泄漏面。
func printConfigSummary(c *models.Config) {
	mask := func(s string) string {
		if s == "" {
			return "<empty>"
		}
		return "<set>"
	}
	fmt.Printf("loaded config summary: env=%s addr=%d mysql=%s@%s:%s/%s mysql-password=%s etcd=%v etcd-password=%s email-secret=%s jwt-secret=%s\n",
		c.System.Env,
		c.System.Addr,
		c.Mysql.Username,
		c.Mysql.Path,
		c.Mysql.Port,
		c.Mysql.Dbname,
		mask(c.Mysql.Password),
		c.Etcd.Endpoints,
		mask(c.Etcd.Password),
		mask(c.Email.Secret),
		mask(c.Auth.JwtSecret),
	)
}

func GetConfigModels() *models.Config {
	return _defaultConfig
}
