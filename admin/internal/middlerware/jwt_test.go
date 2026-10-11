package middlerware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/tmnhs/crony/common/models"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func productionConfig(secret string) *models.Config {
	c := &models.Config{}
	c.System.Env = "production"
	c.Auth.JwtSecret = secret
	return c
}

func testingConfig(secret string) *models.Config {
	c := &models.Config{}
	c.System.Env = "testing"
	c.Auth.JwtSecret = secret
	return c
}

// TestResolveSigningKeyFailsFastInProduction 验证"生产环境没有密钥就拒绝启动"，
// 这是替换掉硬编码密钥后必须保证的 fail-fast 行为。
func TestResolveSigningKeyFailsFastInProduction(t *testing.T) {
	cases := []struct {
		name   string
		secret string
	}{
		{"未配置", ""},
		{"历史硬编码占位值", "S0dEdN9tqG0AAAAHdElNRQfmCgwBDCSd2zTMAAAA"},
		{"过短", "short-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("生产环境 %s 时 resolveSigningKey 必须 panic（fail fast），实际没有", tc.name)
				}
			}()
			resolveSigningKey(productionConfig(tc.secret))
		})
	}
}

func TestResolveSigningKeyUsesConfiguredSecret(t *testing.T) {
	got := string(resolveSigningKey(productionConfig(testSecret)))
	if got != testSecret {
		t.Fatalf("resolveSigningKey() = %q, want 配置的密钥", got)
	}
}

// TestResolveSigningKeyGeneratesRandomInTesting
// 非生产环境未配置密钥时不得回退到任何硬编码值，而是生成进程内随机密钥。
func TestResolveSigningKeyGeneratesRandomInTesting(t *testing.T) {
	first := string(resolveSigningKey(testingConfig("")))
	second := string(resolveSigningKey(testingConfig("")))
	if len(first) < MinSecretLength {
		t.Fatalf("随机密钥长度 %d, want >= %d", len(first), MinSecretLength)
	}
	if first == second {
		t.Fatal("两次解析生成了相同密钥：随机性不足")
	}
	if strings.Contains(first, "S0dEdN9tqG0") {
		t.Fatal("不得使用历史硬编码密钥")
	}
}

func TestTokenExpireDuration(t *testing.T) {
	if got := tokenExpireDuration(nil); got != time.Duration(defaultTokenExpireHours)*time.Hour {
		t.Fatalf("tokenExpireDuration(nil) = %v, want 默认 %d 小时", got, defaultTokenExpireHours)
	}
	cfg := testingConfig(testSecret)
	cfg.Auth.TokenExpireHours = 1
	if got := tokenExpireDuration(cfg); got != time.Hour {
		t.Fatalf("tokenExpireDuration(1h) = %v, want 1h", got)
	}
}

// TestCreateAndParseTokenKeepsRole 确认令牌里带上了角色，
// 这样 RBAC 中间件无需回查数据库即可判断权限。
func TestCreateAndParseTokenKeepsRole(t *testing.T) {
	j := &JWT{SigningKey: []byte(testSecret)}
	token, err := j.CreateToken(j.CreateClaims(BaseClaims{ID: 7, UserName: "root", Role: models.RoleAdmin}))
	if err != nil {
		t.Fatalf("CreateToken() error = %v", err)
	}
	claims, err := j.ParseToken(token)
	if err != nil {
		t.Fatalf("ParseToken() error = %v", err)
	}
	if claims.BaseClaims.ID != 7 || claims.BaseClaims.UserName != "root" || claims.BaseClaims.Role != models.RoleAdmin {
		t.Fatalf("claims = %+v, want id=7 user=root role=admin", claims.BaseClaims)
	}
}

func TestParseTokenRejectsOtherKeys(t *testing.T) {
	signer := &JWT{SigningKey: []byte(testSecret)}
	token, err := signer.CreateToken(signer.CreateClaims(BaseClaims{ID: 1, UserName: "u", Role: models.RoleNormal}))
	if err != nil {
		t.Fatalf("CreateToken() error = %v", err)
	}
	attacker := &JWT{SigningKey: []byte("ffffffffffffffffffffffffffffffff")}
	if _, err := attacker.ParseToken(token); err == nil {
		t.Fatal("使用其他密钥签发的令牌必须被拒绝")
	}
}

func TestParseTokenRejectsExpiredToken(t *testing.T) {
	j := &JWT{SigningKey: []byte(testSecret)}
	claims := CustomClaims{
		BaseClaims: BaseClaims{ID: 1, UserName: "u", Role: models.RoleNormal},
		RegisteredClaims: jwt.RegisteredClaims{
			NotBefore: jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
	}
	token, err := j.CreateToken(claims)
	if err != nil {
		t.Fatalf("CreateToken() error = %v", err)
	}
	if _, err := j.ParseToken(token); err != TokenExpired {
		t.Fatalf("ParseToken() error = %v, want TokenExpired", err)
	}
}

// TestParseTokenRejectsUnsignedToken 防止 alg=none / 非 HMAC 算法的算法混淆攻击。
func TestParseTokenRejectsUnsignedToken(t *testing.T) {
	claims := CustomClaims{
		BaseClaims: BaseClaims{ID: 1, UserName: "attacker", Role: models.RoleAdmin},
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("构造 alg=none 令牌失败: %v", err)
	}
	j := &JWT{SigningKey: []byte(testSecret)}
	if _, err := j.ParseToken(unsigned); err == nil {
		t.Fatal("alg=none 的令牌必须被拒绝")
	}

	// 换成 HS512 签名（密钥相同）也必须在 WithValidMethods 白名单外被拒绝。
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("构造 HS512 令牌失败: %v", err)
	}
	if _, err := j.ParseToken(hs512); err == nil {
		t.Fatal("非白名单签名算法（HS512）的令牌必须被拒绝")
	}
}

func TestParseTokenRejectsGarbage(t *testing.T) {
	j := &JWT{SigningKey: []byte(testSecret)}
	if _, err := j.ParseToken("not-a-token"); err != TokenMalformed {
		t.Fatalf("ParseToken(garbage) error = %v, want TokenMalformed", err)
	}
}

// TestBearerToken 锁定 Authorization 头的两种可接受写法。
//
// 背景：历史实现把整个头当令牌解析，于是 console 这类按 RFC 6750 发送
// `Bearer <jwt>` 的客户端会一直拿到 "That's not even a token"，
// 而 Mock 模式下的前端完全测不出来（这是"只测内存/只测 Mock"的典型盲区）。
func TestBearerToken(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"标准 Bearer", "Bearer abc.def.ghi", "abc.def.ghi"},
		{"小写 bearer", "bearer abc.def.ghi", "abc.def.ghi"},
		{"大写 BEARER", "BEARER abc.def.ghi", "abc.def.ghi"},
		{"历史裸 token", "abc.def.ghi", "abc.def.ghi"},
		{"多余空白", "  Bearer   abc.def.ghi  ", "abc.def.ghi"},
		{"空头", "", ""},
		{"只有 Bearer（有 scheme 无凭据 → 视为未登录）", "Bearer", ""},
		{"Bearer 后为空", "Bearer ", ""},
		{"Bearer 后跟 tab", "Bearer\tabc.def.ghi", "abc.def.ghi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BearerToken(tc.header); got != tc.want {
				t.Fatalf("BearerToken(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

// TestJWTAuthAcceptsBothHeaderForms：中间件层面必须两种写法都能放行，
// 且要留意"没带令牌"仍然被拒（不能因为兼容而放宽鉴权）。
func TestJWTAuthAcceptsBothHeaderForms(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key := LoadSigningKey()
	signer := &JWT{SigningKey: key}
	token, err := signer.CreateToken(signer.CreateClaims(BaseClaims{ID: 9, UserName: "u", Role: models.RoleAdmin}))
	if err != nil {
		t.Fatalf("签发令牌失败: %v", err)
	}

	cases := []struct {
		name    string
		header  string
		wantRun bool
	}{
		{"Bearer 前缀", "Bearer " + token, true},
		{"裸 token", token, true},
		{"无头", "", false},
		{"坏令牌", "Bearer not-a-token", false},
		{"Bearer 前缀 + 坏令牌（不得回退成裸 token 解析）", "Bearer Bearer " + token, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			r := gin.New()
			r.Use(JWTAuth())
			r.GET("/probe", func(c *gin.Context) { ran = true })
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if ran != tc.wantRun {
				t.Fatalf("放行 = %v，want %v（HTTP %d，body=%s）", ran, tc.wantRun, w.Code, w.Body.String())
			}
			if !tc.wantRun && !strings.Contains(w.Body.String(), "code") {
				t.Fatalf("拒绝时必须返回统一错误信封，实际 %s", w.Body.String())
			}
		})
	}
}
