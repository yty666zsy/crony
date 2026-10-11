package handler

import (
	"fmt"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/middlerware"
	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/admin/internal/service"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/crypt"
	"github.com/tmnhs/crony/common/pkg/logger"
)

type UserRouter struct {
}

var defaultUserRouter = new(UserRouter)

func (u *UserRouter) Login(c *gin.Context) {
	var req request.ReqUserLogin
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[user_login] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[user_login] request parameter error", c)
		return
	}
	user, err := service.DefaultUserService.Login(req.UserName, req.Password)
	if err != nil || user == nil || user.ID == 0 {
		// 统一话术，不区分"用户不存在"与"口令错误"，避免用户名枚举。
		logger.GetLogger().Error(fmt.Sprintf("[user_login] login failed for username=%s error=%v", req.UserName, err))
		resp.FailWithMessage(resp.ERROR, "[user_login] username or password is incorrect", c)
		return
	}

	j := middlerware.NewJWT()
	claims := j.CreateClaims(middlerware.BaseClaims{
		ID:       user.ID,
		UserName: user.UserName,
		Role:     user.Role,
	})
	token, err := j.CreateToken(claims)
	if err != nil {
		resp.FailWithMessage(resp.ErrorTokenGenerate, "获取token失败", c)
		return
	}
	resp.OkWithDetailed(resp.RspLogin{
		User:  user,
		Token: token,
	}, "login success", c)
}

// Register 创建账号。
//
// 安全策略（阶段 0 修复"免鉴权自注册管理员"）：
//  1. 客户端传入的 role 一律忽略，不再写入库——这是原实现最严重的提权漏洞；
//  2. 用户表为空时视为"首个管理员引导注册"，该账号强制为管理员；
//  3. 其余情况必须由管理员携带有效令牌调用；除非显式打开
//     auth.allow-public-register（默认关闭），且此时新账号一律为普通用户；
//  4. 口令必须是 bcrypt 哈希，强度校验不通过直接拒绝。
func (u *UserRouter) Register(c *gin.Context) {
	var req request.ReqUserRegister
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[user_register] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[user_register] request parameter error", c)
		return
	}
	req.UserName = strings.TrimSpace(req.UserName)
	if req.UserName == "" {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[user_register] username must not be empty", c)
		return
	}

	userCount, err := service.DefaultUserService.Count()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[user_register] count users error:%v", err))
		resp.FailWithMessage(resp.ERROR, "[user_register] db error", c)
		return
	}
	isBootstrap := userCount == 0
	allowPublicRegister := false
	if cfg := config.GetConfigModels(); cfg != nil {
		allowPublicRegister = cfg.Auth.AllowPublicRegister
	}
	isAdmin := middlerware.IsAdmin(c)

	decision := decideRegister(userCount, isAdmin, allowPublicRegister)
	if !decision.Allowed {
		resp.FailWithMessage(resp.ErrorRegisterClosed, decision.Reason, c)
		return
	}

	if err := crypt.ValidatePasswordStrength(req.Password); err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[user_register] password rejected: "+err.Error(), c)
		return
	}

	user, err := service.DefaultUserService.FindByUserName(req.UserName)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[user_register] db find by username error:%s, error:%v", req.UserName, err))
		resp.FailWithMessage(resp.ERROR, "[user_register] db error", c)
		return
	}
	if user != nil {
		logger.GetLogger().Error(fmt.Sprintf("[user_register] db find by username:%s", req.UserName))
		resp.FailWithMessage(resp.ErrorUserNameExist, "[user_register] the user name has already been used", c)
		return
	}

	hashed, err := crypt.HashPassword(req.Password)
	if err != nil {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[user_register] password rejected: "+err.Error(), c)
		return
	}

	// 角色只由服务端决定：引导注册 = 管理员，其余 = 普通用户。
	// 客户端传入的 req.Role 在此被彻底忽略（原实现直接采用 req.Role，可自注册为管理员）。
	userModel := &models.User{
		UserName: req.UserName,
		Password: hashed,
		Role:     decision.Role,
		Email:    req.Email,
		Created:  time.Now().Unix(),
	}
	insertId, err := userModel.Insert()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[user_register] db insert error:%v", err))
		resp.FailWithMessage(resp.ERROR, "[user_register] db insert error", c)
		return
	}
	userModel.ID = insertId
	logger.GetLogger().Info(fmt.Sprintf("[audit][user_register] operator=%s(admin=%v) target=%s role=%d reason=%s bootstrap=%v",
		operatorName(c), isAdmin, userModel.UserName, decision.Role, decision.Reason, isBootstrap))

	// 不回传口令哈希。
	userModel.Password = ""
	resp.OkWithDetailed(userModel, "register success", c)
}

func (u *UserRouter) Update(c *gin.Context) {
	var req models.User
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[update_user] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[update_user] request parameter error", c)
		return
	}
	if req.ID == 0 {
		resp.FailWithMessage(resp.ErrorRequestParameter, "[update_user] id is required", c)
		return
	}
	// 口令不在这里修改：必须走 change_pw（校验原口令 + bcrypt）。
	// GORM 的 Updates(struct) 会跳过零值字段，因此置空即"不改动"。
	req.Password = ""
	req.Updated = time.Now().Unix()
	err := req.Update()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[update_user] db update error:%v", err))
		resp.FailWithMessage(resp.ERROR, "[update_user] db update error", c)
		return
	}
	logger.GetLogger().Info(fmt.Sprintf("[audit][update_user] operator=%s target_id=%d role=%d", operatorName(c), req.ID, req.Role))
	resp.OkWithMessage("update success", c)
}

func (u *UserRouter) Delete(c *gin.Context) {
	var req request.ByIDS
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[delete_user] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[delete_user] request parameter error", c)
		return
	}
	currentUserID := middlerware.CurrentUserID(c)
	for _, id := range req.IDS {
		if id == currentUserID {
			resp.FailWithMessage(resp.ERROR, "[delete_user] 不能删除当前登录账号", c)
			return
		}
	}
	for _, id := range req.IDS {
		userModel := models.User{ID: id}
		err := userModel.Delete()
		if err != nil {
			logger.GetLogger().Error(fmt.Sprintf("[delete_user] db error:%v", err))
		}
	}
	logger.GetLogger().Info(fmt.Sprintf("[audit][delete_user] operator=%s target_ids=%v", operatorName(c), req.IDS))
	resp.OkWithMessage("delete success", c)
}

func (u *UserRouter) ChangePassword(c *gin.Context) {
	var req request.ReqChangePassword
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[change_password] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[change_password] request parameter error", c)
		return
	}
	claims := middlerware.GetUserInfo(c)
	if claims == nil {
		resp.FailWithDetailed(resp.ERROR, gin.H{"reload": true}, "未登录或非法访问", c)
		return
	}
	err := service.DefaultUserService.ChangePassword(claims.BaseClaims.ID, req.Password, req.NewPassword)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[change_password] error:%v", err))
		resp.FailWithMessage(resp.ERROR, "[change_password] "+err.Error(), c)
		return
	}
	logger.GetLogger().Info(fmt.Sprintf("[audit][change_password] operator=%s", operatorName(c)))
	resp.OkWithMessage("update success", c)
}

func (u *UserRouter) FindById(c *gin.Context) {
	var req request.ByID
	if err := c.ShouldBindQuery(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[find_user] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[find_user] request parameter error", c)
		return
	}
	// 非管理员只能查自己，避免越权读取他人邮箱等信息。
	if !middlerware.IsAdmin(c) {
		req.ID = middlerware.CurrentUserID(c)
	}
	if req.ID == 0 {
		req.ID = middlerware.CurrentUserID(c)
	}
	userModel := models.User{ID: req.ID}
	err := userModel.FindById()
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[find_user] db  error:%v", err))
		resp.FailWithMessage(resp.ERROR, "[find_user] db  error", c)
		return
	}
	resp.OkWithDetailed(userModel, "find success", c)
}

func (u *UserRouter) Search(c *gin.Context) {
	var req request.ReqUserSearch
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[search_user] request parameter error:%s", err.Error()))
		resp.FailWithMessage(resp.ErrorRequestParameter, "[search_user] request parameter error", c)
		return
	}
	req.Check()
	users, total, err := service.DefaultUserService.Search(&req)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("[search_user] db error:%v", err))
		resp.FailWithMessage(resp.ERROR, "[search_user] db error", c)
		return
	}
	for i := range users {
		users[i].Password = ""
	}
	resp.OkWithDetailed(resp.PageResult{
		List:     users,
		Total:    total,
		PageSize: req.PageSize,
		Page:     req.Page,
	}, "search success", c)
}

// registerDecision 描述"本次注册请求是否允许，以及新账号的角色"。
type registerDecision struct {
	Allowed bool
	Role    int
	Reason  string
}

// decideRegister 是注册策略的纯函数形式（便于单元测试）。
//
// 规则：
//  1. 用户表为空 => 首个管理员引导注册，允许且强制管理员角色；
//  2. 已存在用户 + 调用方是管理员 => 允许，新账号为普通用户（管理员身份需走 /user/update）；
//  3. 已存在用户 + 显式打开 allow-public-register => 允许，新账号仍为普通用户；
//  4. 其余 => 拒绝。
//
// 关键点：无论哪条分支，新账号角色都不来自客户端请求体，
// 从而消除旧版"免鉴权自注册管理员"的提权漏洞。
func decideRegister(userCount int64, isAdmin, allowPublicRegister bool) registerDecision {
	if userCount == 0 {
		return registerDecision{Allowed: true, Role: models.RoleAdmin, Reason: "首个管理员引导注册"}
	}
	if isAdmin {
		return registerDecision{Allowed: true, Role: models.RoleNormal, Reason: "管理员创建账号"}
	}
	if allowPublicRegister {
		return registerDecision{Allowed: true, Role: models.RoleNormal, Reason: "显式开启公开注册"}
	}
	return registerDecision{Allowed: false, Reason: "注册已关闭：仅管理员可创建账号"}
}

// operatorName 返回审计日志中的操作者标识（未登录时返回 anonymous）。
func operatorName(c *gin.Context) string {
	if name := middlerware.CurrentUserName(c); name != "" {
		return name
	}
	return "anonymous"
}
