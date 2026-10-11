package middlerware

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/logger"
	"golang.org/x/sync/singleflight"
)

// Custom claims structure
type CustomClaims struct {
	BaseClaims
	BufferTime int64
	jwt.RegisteredClaims
}

type BaseClaims struct {
	ID       int
	UserName string
	// Role 登录用户角色。放进 claims 后，鉴权中间件无需回查数据库，
	// 也避免"每个请求都查一次 user 表"的开销。
	Role int
}

type JWT struct {
	SigningKey []byte
}

var (
	TokenExpired     = errors.New("Token is expired")
	TokenNotValidYet = errors.New("Token not active yet")
	TokenMalformed   = errors.New("That's not even a token")
	TokenInvalid     = errors.New("Couldn't handle this token")

	// ErrSigningMethod 令牌签名算法不在允许列表内（防止 alg 混淆攻击）。
	ErrSigningMethod = errors.New("unexpected signing method")
)

const (
	// MinSecretLength 签名密钥最小长度。低于该长度视为弱密钥。
	MinSecretLength = 32
	// defaultTokenExpireHours 未配置时的令牌有效期（7 天，与原实现一致）。
	defaultTokenExpireHours = 168
)

// 历史硬编码密钥与常见占位值：命中即视为"未配置"。
var placeholderSecrets = map[string]struct{}{
	"S0dEdN9tqG0AAAAHdElNRQfmCgwBDCSd2zTMAAAA": {},
}

var (
	control         = &singleflight.Group{}
	signingKeyOnce  sync.Once
	signingKeyBytes []byte
)

// LoadSigningKey 返回进程级 JWT 签名密钥，只解析一次。
//
// 取值优先级：环境变量 CRONY_AUTH_JWT_SECRET > 配置文件 auth.jwt-secret。
// 安全策略：
//   - 生产环境（system.env = production）未配置或命中历史硬编码占位值 → 直接 panic，
//     拒绝以"任何人可伪造令牌"的状态启动（fail fast）；
//   - 生产环境密钥长度 < MinSecretLength → panic；
//   - 非生产环境未配置 → 生成进程内随机会话密钥并醒目告警（绝不回退到硬编码密钥），
//     代价是进程重启后登录态失效，这是有意的取舍。
func LoadSigningKey() []byte {
	signingKeyOnce.Do(func() {
		signingKeyBytes = resolveSigningKey(config.GetConfigModels())
	})
	return signingKeyBytes
}

// resolveSigningKey 是纯函数形式的密钥解析逻辑（便于单元测试）：
// 输入配置，输出要使用的签名密钥。
//
// 生产环境（system.env = production）未配置、命中历史硬编码占位值或长度不足
// MinSecretLength 时直接 panic，拒绝以"任何人可伪造令牌"的状态启动（fail fast）；
// 非生产环境未配置时生成进程内随机会话密钥并告警，绝不回退到硬编码密钥。
func resolveSigningKey(cfg *models.Config) []byte {
	var secret, env string
	if cfg != nil {
		secret = strings.TrimSpace(cfg.Auth.JwtSecret)
		env = strings.TrimSpace(cfg.System.Env)
	}
	production := env == string(config.EnvProduction)

	if secret != "" {
		if _, isPlaceholder := placeholderSecrets[secret]; !isPlaceholder {
			if len(secret) < MinSecretLength {
				message := fmt.Sprintf("auth.jwt-secret 长度不足 %d 字节（当前 %d 字节），存在被离线爆破并伪造令牌的风险", MinSecretLength, len(secret))
				if production {
					panic("crony: " + message + "；请设置 CRONY_AUTH_JWT_SECRET 为 >= 32 字节的随机串")
				}
				fmt.Fprintln(os.Stderr, "[crony][WARN] "+message)
			}
			return []byte(secret)
		}
		fmt.Fprintln(os.Stderr, "[crony][WARN] auth.jwt-secret 仍是历史硬编码占位值，已忽略。")
	}

	if production {
		panic("crony: 生产环境必须显式配置 JWT 签名密钥，请设置环境变量 CRONY_AUTH_JWT_SECRET（>= 32 字节随机串）或配置文件 auth.jwt-secret")
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		panic("crony: 生成随机会话密钥失败: " + err.Error())
	}
	fmt.Fprintln(os.Stderr, "[crony][WARN] 未配置 auth.jwt-secret，已生成进程内随机会话密钥：进程重启后所有登录态失效。生产环境请显式配置 CRONY_AUTH_JWT_SECRET。")
	return []byte(hex.EncodeToString(buf))
}

func NewJWT() *JWT {
	return &JWT{SigningKey: LoadSigningKey()}
}

// tokenExpireDuration 计算令牌有效期（纯函数，便于单元测试）。
func tokenExpireDuration(cfg *models.Config) time.Duration {
	hours := int64(defaultTokenExpireHours)
	if cfg != nil && cfg.Auth.TokenExpireHours > 0 {
		hours = cfg.Auth.TokenExpireHours
	}
	return time.Duration(hours) * time.Hour
}

func (j *JWT) CreateClaims(baseClaims BaseClaims) CustomClaims {
	now := time.Now()
	claims := CustomClaims{
		BaseClaims: baseClaims,
		BufferTime: 86400, // buffer time 1 day：前端可在过期前用旧令牌换新令牌
		RegisteredClaims: jwt.RegisteredClaims{
			NotBefore: jwt.NewNumericDate(now.Add(-1000 * time.Second)), // effective time of signature
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenExpireDuration(config.GetConfigModels()))),
			Issuer:    "tmnhs",
		},
	}
	return claims
}

// create a token
func (j *JWT) CreateToken(claims CustomClaims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(j.SigningKey)
}

// Replacing old token with new token using merging and origin-pull to avoid concurrency problems
func (j *JWT) CreateTokenByOldToken(oldToken string, claims CustomClaims) (string, error) {
	v, err, _ := control.Do("JWT:"+oldToken, func() (interface{}, error) {
		return j.CreateToken(claims)
	})
	return v.(string), err
}

func (j *JWT) ParseToken(tokenString string) (*CustomClaims, error) {
	token, err := jwt.ParseWithClaims(
		tokenString,
		&CustomClaims{},
		func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, ErrSigningMethod
			}
			return j.SigningKey, nil
		},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenMalformed):
			return nil, TokenMalformed
		case errors.Is(err, jwt.ErrTokenExpired):
			return nil, TokenExpired
		case errors.Is(err, jwt.ErrTokenNotValidYet):
			return nil, TokenNotValidYet
		case errors.Is(err, ErrSigningMethod):
			return nil, TokenInvalid
		default:
			return nil, TokenInvalid
		}
	}
	if token == nil {
		return nil, TokenInvalid
	}
	if claims, ok := token.Claims.(*CustomClaims); ok && token.Valid {
		return claims, nil
	}
	return nil, TokenInvalid
}

func GetClaims(c *gin.Context) (*CustomClaims, error) {
	token := c.Request.Header.Get("Authorization")
	j := NewJWT()
	claims, err := j.ParseToken(token)
	if err != nil {
		logger.GetLogger().Error("Failed to obtain parsing information from jwt from Context of Gin. Please check whether Authorization exists in the request header and whether claims is the specified structure.")
	}
	return claims, err
}

// Get the user roles parsed from jwt from the Context of Gin
func GetUserInfo(c *gin.Context) *CustomClaims {
	if claims, exists := c.Get("claims"); !exists {
		if cl, err := GetClaims(c); err != nil {
			return nil
		} else {
			return cl
		}
	} else {
		waitUse := claims.(*CustomClaims)
		return waitUse
	}
}

func JWTAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// We have jwt authentication header information to return token information when Authorization logs in. Here,
		//the front end needs to store the token in cookie or local localStorage,
		//but you need to negotiate the expiration time with the back end.
		//You can agree to refresh the token or log in again.
		token := c.Request.Header.Get("Authorization")
		if token == "" {
			resp.FailWithDetailed(resp.ERROR, gin.H{"reload": true}, "未登录或非法访问", c)
			c.Abort()
			return
		}
		j := NewJWT()
		claims, err := j.ParseToken(token)
		if err != nil {
			if err == TokenExpired {
				resp.FailWithDetailed(resp.ERROR, gin.H{"reload": true}, "授权已过期", c)
				c.Abort()
				return
			}
			resp.FailWithDetailed(resp.ERROR, gin.H{"reload": true}, err.Error(), c)
			c.Abort()
			return
		}
		c.Set("claims", claims)
		c.Next()
	}
}
