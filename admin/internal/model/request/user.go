package request

// 口令字段的校验口径（唯一事实来源 = `common/pkg/crypt`）。
//
// 旧写法给口令加了 `min=4,max=20,alphanum`，这等于在**接口层另立了一套策略**，
// 与平台真正的强度策略（`crypt.ValidatePasswordStrength`：>=8 位 + 弱口令黑名单）冲突，
// 于是同一串口令"能建账号、却登录不进去"：
//
//   - 引导管理员口令只过 `crypt.HashPassword`（允许符号、上限 72 字节），
//     而登录接口的 `alphanum` 会把 `change-me-strong-password` 这类口令判为参数错误（code=1001）；
//   - 反过来，`max=20` 又把 21 位以上的合规口令挡在门外。
//
// 现在 binding 只负责"这串东西存在且不超出 bcrypt 的 72 字节上限"，
// 长度下限/弱口令/字符集一律由 `crypt` 判定，错误信息也只有一处，不再互相打架。
type (
	ReqUserLogin struct {
		UserName string `json:"username" form:"username" binding:"required,min=2,max=20"`
		Password string `json:"password" form:"password" binding:"required,min=1,max=72"`
	}
	ReqUserRegister struct {
		UserName string `json:"username" form:"username" binding:"required,min=2,max=20"`
		Password string `json:"password" form:"password" binding:"required,min=1,max=72"`
		Email    string `json:"email" form:"email"`
		Role     int    `json:"role" form:"email"`
	}
	// Modify password structure
	ReqChangePassword struct {
		// 旧口令：只校验非空（历史账号的口令可能是任意字符集，不能再次被策略挡住）
		Password string `json:"password" binding:"required,min=1,max=72"`
		// 新口令：强度由 service 侧的 `crypt.HashPassword` 判定（>=8 位 + 弱口令黑名单）
		NewPassword string `json:"new_password" binding:"required,min=1,max=72"`
	}
	ReqUserSearch struct {
		PageInfo
		ID       int    `json:"id" form:"id"`
		UserName string `json:"username" form:"username"`
		Email    string `json:"email" form:"email"`
		Role     int    `json:"role" form:"email"`
	}
)
