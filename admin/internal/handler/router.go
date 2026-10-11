package handler

import (
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/tmnhs/crony/admin/internal/middlerware"
	"github.com/tmnhs/crony/admin/internal/model/resp"
	"github.com/tmnhs/crony/common/pkg/logger"
)

func RegisterRouters(r *gin.Engine) {
	r.Use(middlerware.Cors())

	configRoute(r)

	configNoRoute(r)
}

// configRoute 注册全部业务路由。
//
// 鉴权分层（阶段 0 引入 RBAC）：
//   - 公开：/ping、/login、/register（register 内部再判定是否为"首个管理员引导"，
//     非引导场景必须携带管理员令牌，详见 handler/user.go）；
//   - 登录即可（普通用户）：任务、脚本查询、节点查询、统计、自身信息与改密；
//   - 仅管理员：用户管理、节点删除、脚本增删（这些都是影响面最大的操作）。
func configRoute(r *gin.Engine) {

	hello := r.Group("/ping")
	{
		hello.GET("", func(c *gin.Context) {
			c.JSON(200, "pong")
		})
		hello.POST("", func(c *gin.Context) {
			type Hello struct {
				Name string `json:"name" form:"name"`
			}
			var h Hello
			var err error
			err = c.ShouldBindJSON(&h)
			if err != nil {
				c.JSON(resp.ERROR, err.Error())
			}
			c.JSON(200, "hello,"+h.Name)
		})
	}

	base := r.Group("")
	{
		base.POST("register", defaultUserRouter.Register)
		base.POST("login", defaultUserRouter.Login)
	}

	stat := r.Group("/statis")
	stat.Use(middlerware.JWTAuth())
	{
		stat.GET("today", defaultStatRouter.GetTodayStatistics)
		stat.GET("week", defaultStatRouter.GetWeekStatistics)
		stat.GET("system", defaultStatRouter.GetSystemInfo)

	}

	job := r.Group("/job")
	job.Use(middlerware.JWTAuth())
	{
		job.POST("add", defaultJobRouter.CreateOrUpdate)
		job.POST("del", defaultJobRouter.Delete)
		job.GET("find", defaultJobRouter.FindById)
		job.POST("search", defaultJobRouter.Search)
		job.POST("log", defaultJobRouter.SearchLog)
		job.POST("once", defaultJobRouter.Once)
		// 阶段 1 恢复：节点侧已实现基于 context + 进程树终止的取消传播
		job.POST("kill", defaultJobRouter.Kill)

		// 阶段 1（任务模型 2.0）执行记录读取面：
		// job_run 带幂等键、触发来源、状态机、策略快照与步骤明细
		run := job.Group("/run")
		{
			run.POST("search", defaultJobRunRouter.Search)
			run.GET("find", defaultJobRunRouter.Find)
			run.POST("steps", defaultJobRunRouter.Steps)
			run.GET("overview", defaultJobRunRouter.Overview)
			// 阶段 2 收尾：执行事实订阅（SSE）。轮询语义在服务端，前端只消费推送。
			// 注意：这是 GET + JWT，浏览器侧必须用 fetch 流式读取（EventSource 不能带 Authorization 头）。
			run.GET("stream", defaultJobRunRouter.RunStream)
		}
	}

	// 阶段 2（Agent 内核）：会话/诊断/工具清单/自然语言建任务。
	//
	// 权限：注册到与其它业务接口相同的"登录即可"层级。
	// 但注意：起草/确认会消耗模型额度并创建任务定义，因此：
	//   - 起草不落 job（只落会话）；确认创建的任务一律 status=0（未分配），不会自动执行；
	//   - 真正的执行放权仍由既有"分配节点"动作控制（RequireAdmin 的路径不变）。
	agentGroup := r.Group("/agent")
	agentGroup.Use(middlerware.JWTAuth())
	{
		agentGroup.POST("/session/search", defaultAgentRouter.SessionSearch)
		agentGroup.POST("/session/find", defaultAgentRouter.SessionFind)
		agentGroup.POST("/diagnosis/find", defaultAgentRouter.DiagnosisFind)
		agentGroup.POST("/diagnosis/run", defaultAgentRouter.DiagnosisRun)
		agentGroup.POST("/tool/list", defaultAgentRouter.ToolList)
		agentGroup.POST("/nljob/draft", defaultAgentRouter.NLJobDraft)
		agentGroup.POST("/nljob/confirm", defaultAgentRouter.NLJobConfirm)
	}

	user := r.Group("/user")
	user.Use(middlerware.JWTAuth())
	{
		// 登录用户可操作自己的信息与口令
		user.POST("change_pw", defaultUserRouter.ChangePassword)
		user.GET("find", defaultUserRouter.FindById)

		// 以下为用户管理动作，仅管理员
		admin := user.Group("")
		admin.Use(middlerware.RequireAdmin())
		{
			admin.POST("del", defaultUserRouter.Delete)
			admin.POST("update", defaultUserRouter.Update)
			admin.POST("search", defaultUserRouter.Search)
		}
	}

	node := r.Group("/node")
	node.Use(middlerware.JWTAuth())
	{
		node.POST("search", defaultNodeRouter.Search)
	}
	nodeAdmin := r.Group("/node")
	nodeAdmin.Use(middlerware.JWTAuth(), middlerware.RequireAdmin())
	{
		nodeAdmin.POST("del", defaultNodeRouter.Delete)
	}

	script := r.Group("/script")
	script.Use(middlerware.JWTAuth())
	{
		script.GET("find", defaultScriptRouter.FindById)
		script.POST("search", defaultScriptRouter.Search)
	}
	scriptAdmin := r.Group("/script")
	scriptAdmin.Use(middlerware.JWTAuth(), middlerware.RequireAdmin())
	{
		scriptAdmin.POST("add", defaultScriptRouter.CreateOrUpdate)
		scriptAdmin.POST("del", defaultScriptRouter.Delete)
	}
}

// configNoRoute 挂载旧版前端（Vue2 打包产物 ./dist）。
//
// 旧实现无条件执行 r.LoadHTMLGlob("./dist/*.html")：gin 的模板 glob 匹配不到任何文件时
// 会直接 panic，因此在"只跑 API / 还没构建前端 / 容器里没有 dist"的环境里，
// admin 服务会启动失败。现在先探测 dist，缺失时只注册探活接口并打印告警。
func configNoRoute(r *gin.Engine) {
	if _, err := os.Stat("./dist/index.html"); err != nil {
		logger.GetLogger().Warn("未找到 ./dist/index.html：跳过旧版前端静态资源挂载，仅提供 API 服务。")
		r.GET("/", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{
				"service": "crony-admin",
				"ui":      false,
				"message": "旧版前端未构建（./dist 不存在）。AI Agent 控制台在阶段 1 交付后挂载到 /console。",
			})
		})
		return
	}
	r.LoadHTMLGlob("./dist/*.html") // npm打包成dist的路径
	r.StaticFile("favicon.ico", "./dist/favicon.ico")
	r.Static("/css", "./dist/css")         // dist里面的静态资源
	r.Static("/fonts", "./dist/fonts")     // dist里面的静态资源
	r.Static("/js", "./dist/js")           // dist里面的静态资源
	r.Static("/img", "./dist/img")         // dist里面的静态资源
	r.StaticFile("/", "./dist/index.html") // 前端网页入口页面
}
