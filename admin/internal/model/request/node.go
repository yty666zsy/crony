package request

type (
	ReqNodeSearch struct {
		PageInfo
		Hostname string `json:"hostname" form:"hostname"` // 主机名（模糊匹配）
		IP       string `json:"ip" form:"ip"`             // node ip（模糊匹配）
		UUID     string `json:"uuid" form:"uuid"`
		UpTime   int64  `json:"up" form:"up"`         // 启动时间
		Status   int    `json:"status" form:"status"` // 状态
	}
	ByUUID struct {
		UUID string `json:"uuid" form:"uuid"`
	}
)
