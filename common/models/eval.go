package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 阶段 5「回放评测台」的两张表。
//
// 设计取舍：
//   - **样本与跑批分表**。样本（eval_case）是"要考什么"，跑批（eval_run）是"某次考了多少"。
//     合成一张表会让"改一条断言"覆盖掉历史跑批结果，历史就不可比了。
//   - **期望值存一列 JSON**（expect_json）。断言种类还会增加（现在 8 种），
//     每加一种就加一列会把表撑宽，而期望值永远是"整体读写、不按单个断言查"。
//     这与 job.steps_json 存编排是同一取舍：编排/期望是**一个整体**，不是可查询的实体。
//   - **报告存一列 JSON**（report_json）。报告只用于展示与回溯，没有"按报告里的字段查询"的需求。
const (
	// CronyEvalCaseTableName 评测样本（种子评测集）表名。
	CronyEvalCaseTableName = "eval_case"
	// CronyEvalRunTableName 评测跑批结果表名。
	CronyEvalRunTableName = "eval_run"
)

// 单条样本的判定结果。**三态**，与验收用例同口径：
//   - passed  通过了全部断言
//   - failed  至少一条断言不通过（真信号）
//   - skipped 没有可比对的会话 / 样本被停用（既不算通过也不算失败）
//
// 把 skipped 单独拎出来是刻意的：把"没考到"算成"通过"是自欺，
// 算成"失败"则是误报，两者都会让评测失去可信度。
const (
	EvalStatusPassed  = "passed"
	EvalStatusFailed  = "failed"
	EvalStatusSkipped = "skipped"
)

// EvalCaseSourceSeed 样本来源：内置种子集。
const EvalCaseSourceSeed = "seed"

// EvalModeReplay 跑批模式：对已录制轨迹回放打分（目前唯一模式）。
//
// 为什么先做回放：它**不需要活的模型**（历史轨迹已经在库里），
// 因此在没有 LLM 凭据的环境里也能真跑、真出结论。
const EvalModeReplay = "replay"

// EvalCase 一条评测样本。
type EvalCase struct {
	ID int64 `json:"id" gorm:"column:id;primary_key;auto_increment"`
	// CaseKey 稳定业务键（如 seed-diagnose-readonly）。用业务键做唯一键而不是自增 id，
	// 是为了让"同一条断言在多次同步之间保持同一身份"——否则每次同步都会产生新行，
	// 历史跑批结果就挂不到同一条断言上。
	CaseKey    string `json:"case_key" gorm:"size:128;column:case_key;not null;uniqueIndex:uk_eval_case_key"`
	SetName    string `json:"set_name" gorm:"size:64;column:set_name;not null;default:''"`
	SetVersion string `json:"set_version" gorm:"size:64;column:set_version;not null;default:''"`
	// Scenario 场景说明（进报告，给人看）。
	Scenario string `json:"scenario" gorm:"size:512;column:scenario"`
	// SessionPrefix 会话选择规则：按 agent_session.session_id 前缀匹配；空 = 所有会话。
	SessionPrefix string `json:"session_prefix" gorm:"size:128;column:session_prefix"`
	// ExpectJSON 期望值（序列化后的 eval.Case，见 common/pkg/eval）。
	ExpectJSON string `json:"expect_json" gorm:"type:text;column:expect_json"`
	Enabled    bool   `json:"enabled" gorm:"column:enabled;not null;default:true"`
	Source     string `json:"source" gorm:"size:32;column:source;not null;default:'manual'"`
	Created    int64  `json:"created" gorm:"column:created;not null;default:0"`
	Updated    int64  `json:"updated" gorm:"column:updated;not null;default:0"`
}

func (c *EvalCase) TableName() string { return CronyEvalCaseTableName }

// evalCaseLimits 有长度上限的字符字段，与 struct tag 的 size 一一对应。
//
// 与 AgentSession 同样的教训：gorm 的 size tag **只在建表时生效，写入时不校验**，
// 而 MySQL 严格模式会直接报 1406。所以入库前自己挡一道。
//
// 注意这里用取值函数而不是直接把值塞进来：字段是**入库前**才被赋值的，
// 提前取值的表会永远校验到空串（这个坑第一次就踩到了，见 eval_test.go 的超长用例）。
var evalCaseLimits = []struct {
	Field  string
	Label  string
	MaxLen int
	Value  func(*EvalCase) string
}{
	{"case_key", "业务键", 128, func(c *EvalCase) string { return c.CaseKey }},
	{"set_name", "集合名", 64, func(c *EvalCase) string { return c.SetName }},
	{"set_version", "集合版本", 64, func(c *EvalCase) string { return c.SetVersion }},
	{"scenario", "场景说明", 512, func(c *EvalCase) string { return c.Scenario }},
	{"session_prefix", "会话前缀", 128, func(c *EvalCase) string { return c.SessionPrefix }},
	{"source", "来源", 32, func(c *EvalCase) string { return c.Source }},
}

// Validate 入库前的自校验。
func (c *EvalCase) Validate() error {
	if c == nil {
		return fmt.Errorf("评测样本为空")
	}
	for _, l := range evalCaseLimits {
		if n := len([]rune(l.Value(c))); n > l.MaxLen {
			return fmt.Errorf("评测样本 %s 超长：%d > %d", l.Label, n, l.MaxLen)
		}
	}
	if strings.TrimSpace(c.CaseKey) == "" {
		return fmt.Errorf("评测样本缺少 case_key")
	}
	if strings.TrimSpace(c.SetVersion) == "" {
		return fmt.Errorf("评测样本 %s 缺少 set_version（版本是可比性的前提）", c.CaseKey)
	}
	// 期望值必须是**合法 JSON 对象**：写坏了要在入库时就挡住，
	// 而不是等到跑批时 JSON 解析失败——那时它会被误当成"这条样本没通过"。
	if strings.TrimSpace(c.ExpectJSON) == "" {
		return fmt.Errorf("评测样本 %s 缺少 expect_json", c.CaseKey)
	}
	var probe map[string]interface{}
	if err := json.Unmarshal([]byte(c.ExpectJSON), &probe); err != nil {
		return fmt.Errorf("评测样本 %s 的 expect_json 不是合法 JSON 对象：%w", c.CaseKey, err)
	}
	return nil
}

// SaveEvalCase 写入或更新一条样本（按 case_key 去重）。
//
// 返回 created 表示这次是不是新建的，用于同步时给出"新增 N 条 / 更新 M 条"的真实计数。
func SaveEvalCase(db *gorm.DB, c *EvalCase) (bool, error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	now := time.Now().Unix()
	c.Updated = now
	if c.Created == 0 {
		c.Created = now
	}

	// 先探一次是否已存在：只影响返回值（新建/更新），不影响数据正确性。
	var existing int64
	if err := db.Table(CronyEvalCaseTableName).
		Select("id").Where("case_key = ?", c.CaseKey).Limit(1).Scan(&existing).Error; err != nil {
		return false, err
	}
	created := existing == 0

	err := db.Table(CronyEvalCaseTableName).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "case_key"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"set_name":       c.SetName,
			"set_version":    c.SetVersion,
			"scenario":       c.Scenario,
			"session_prefix": c.SessionPrefix,
			"expect_json":    c.ExpectJSON,
			"enabled":        c.Enabled,
			"source":         c.Source,
			"updated":        now,
		}),
	}).Create(c).Error
	if err != nil {
		return false, err
	}
	return created, nil
}

// EvalRun 一次跑批的汇总。
//
// 计数自洽是硬要求（passed+failed+skipped == cases）：一份自相矛盾的报告
// 比没有报告更危险——它会让人以为"评测过了"。
type EvalRun struct {
	ID         int64   `json:"id" gorm:"column:id;primary_key;auto_increment"`
	SetName    string  `json:"set_name" gorm:"size:64;column:set_name;not null;default:''"`
	SetVersion string  `json:"set_version" gorm:"size:64;column:set_version;not null;default:''"`
	Mode       string  `json:"mode" gorm:"size:32;column:mode;not null;default:'replay'"`
	Trigger    string  `json:"trigger" gorm:"size:32;column:trigger;not null;default:'manual'"`
	Cases      int     `json:"cases" gorm:"column:cases;not null;default:0"`
	Passed     int     `json:"passed" gorm:"column:passed;not null;default:0"`
	Failed     int     `json:"failed" gorm:"column:failed;not null;default:0"`
	Skipped    int     `json:"skipped" gorm:"column:skipped;not null;default:0"`
	Sessions   int     `json:"sessions" gorm:"column:sessions;not null;default:0"`
	Score      float64 `json:"score" gorm:"column:score;not null;default:0"`
	// ReportJSON 完整报告（逐样本断言明细），供前端/审计回溯。
	ReportJSON string `json:"report_json" gorm:"type:longtext;column:report_json"`
	StartedAt  int64  `json:"started_at" gorm:"column:started_at;not null;default:0"`
	FinishedAt int64  `json:"finished_at" gorm:"column:finished_at;not null;default:0"`
	DurationMs int64  `json:"duration_ms" gorm:"column:duration_ms;not null;default:0"`
	Created    int64  `json:"created" gorm:"column:created;not null;default:0"`
}

func (r *EvalRun) TableName() string { return CronyEvalRunTableName }

// Validate 入库前的自校验。
func (r *EvalRun) Validate() error {
	if r == nil {
		return fmt.Errorf("评测跑批为空")
	}
	if strings.TrimSpace(r.SetVersion) == "" {
		return fmt.Errorf("评测跑批缺少 set_version")
	}
	if len([]rune(r.SetName)) > 64 || len([]rune(r.Mode)) > 32 || len([]rune(r.Trigger)) > 32 {
		return fmt.Errorf("评测跑批字段超长")
	}
	if r.Passed+r.Failed+r.Skipped != r.Cases {
		return fmt.Errorf("评测跑批计数不自洽：passed(%d)+failed(%d)+skipped(%d) ≠ cases(%d)",
			r.Passed, r.Failed, r.Skipped, r.Cases)
	}
	return nil
}

// InsertEvalRun 落库一次跑批结果。
func InsertEvalRun(db *gorm.DB, r *EvalRun) (int64, error) {
	if err := r.Validate(); err != nil {
		return 0, err
	}
	if r.Created == 0 {
		r.Created = time.Now().Unix()
	}
	if err := db.Table(CronyEvalRunTableName).Create(r).Error; err != nil {
		return 0, err
	}
	return r.ID, nil
}
