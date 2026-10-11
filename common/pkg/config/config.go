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
