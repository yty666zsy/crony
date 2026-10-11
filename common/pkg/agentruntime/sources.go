package agentruntime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/llm"
	"github.com/tmnhs/crony/common/pkg/tools"
	"gorm.io/gorm"
)

func db() *gorm.DB { return dbclient.GetMysqlDB() }

// ensureDB 检查数据库是否已初始化。
//
// 为什么必须显式检查：dbclient.GetMysqlDB() 在未初始化时**返回 nil 并只写日志**，
// 直接调用 .Table() 会 panic。Agent 运行时在"未配置数据库"的环境（单元测试、
// 只读诊断静态部署）里必须给出可读错误，而不是崩掉整个进程。
func ensureDB() error {
	if db() == nil {
		return fmt.Errorf("数据库未初始化（dbclient.Init 未调用）")
	}
	return nil
}

// DBLogSource 基于数据库的日志数据源（admin 与 node 都能用）。
//
// 覆盖两张表，因为历史数据跨了两个模型：
//   - job_log：旧模型的执行结果（有 output/success/retry_times）；
//   - job_run_step：新模型的步骤明细（有 stdout 摘要与错误），比 job_log 精确到步骤。
//
// 证据定位（Ref）刻意采用 `<表>:<主键>` 形式，例如 `job_log:812`、
// `job_run_step:j12-t1791708060#0`：既能定位到行，也能被模型的引用校验逐字匹配。
type DBLogSource struct {
	// MaxScan 单次检索最多从每张表扫描多少行（防止全表扫）。
	MaxScan int
}

func (s *DBLogSource) maxScan() int {
	if s.MaxScan > 0 {
		return s.MaxScan
	}
	return 200
}

// SearchLogs 实现 tools.LogSource。
func (s *DBLogSource) SearchLogs(ctx context.Context, q tools.LogQuery) ([]tools.LogHit, error) {
	if db() == nil {
		return nil, fmt.Errorf("数据库未初始化")
	}
	since := time.Now().Add(-time.Duration(q.SinceMinutes) * time.Minute)
	hits := make([]tools.LogHit, 0, 8)

	// 1) job_log
	logQ := db().WithContext(ctx).Table(models.CronyJobLogTableName)
	if q.JobID > 0 {
		logQ = logQ.Where("job_id = ?", q.JobID)
	}
	logQ = logQ.Where("start_time >= ?", since.Unix())
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		logQ = logQ.Where("(output LIKE ? OR command LIKE ? OR name LIKE ?)", like, like, like)
	}
	logs := make([]*models.JobLog, 0, 20)
	if err := logQ.Order("id DESC").Limit(s.maxScan()).Find(&logs).Error; err != nil {
		return nil, fmt.Errorf("查询 job_log 失败: %w", err)
	}
	for _, l := range logs {
		hits = append(hits, tools.LogHit{
			Source:    "job_log",
			Ref:       fmt.Sprintf("job_log:%d", l.ID),
			Line:      llm.Truncate(llm.Redact(fmt.Sprintf("job[%d] %s success=%v retry=%d output=%s", l.JobId, l.Name, l.Success, l.RetryTimes, l.Output)), 500),
			Timestamp: time.Unix(l.StartTime, 0),
		})
		if len(hits) >= q.Limit {
			return hits, nil
		}
	}

	// 2) job_run_step
	stepQ := db().WithContext(ctx).Table(models.CronyJobRunStepTableName)
	if r := strings.TrimSpace(q.RunID); r != "" {
		stepQ = stepQ.Where("run_id = ?", r)
	} else if q.JobID > 0 {
		// 该任务最近的 run_id 集合（否则无法按 job 过滤步骤表）
		runs := make([]string, 0, 10)
		if err := db().Table(models.CronyJobRunTableName).
			Where("job_id = ?", q.JobID).Order("id DESC").Limit(10).Pluck("run_id", &runs).Error; err != nil {
			return nil, fmt.Errorf("查询 run_id 失败: %w", err)
		}
		if len(runs) == 0 {
			return hits, nil
		}
		stepQ = stepQ.Where("run_id IN ?", runs)
	}
	stepQ = stepQ.Where("created >= ?", since.Unix())
	if kw := strings.TrimSpace(q.Keyword); kw != "" {
		like := "%" + kw + "%"
		stepQ = stepQ.Where("(output LIKE ? OR error_msg LIKE ? OR name LIKE ?)", like, like, like)
	}
	steps := make([]*models.JobRunStep, 0, 20)
	if err := stepQ.Order("id DESC").Limit(s.maxScan()).Find(&steps).Error; err != nil {
		return nil, fmt.Errorf("查询 job_run_step 失败: %w", err)
	}
	for _, st := range steps {
		hits = append(hits, tools.LogHit{
			Source: "job_run_step",
			Ref:    fmt.Sprintf("job_run_step:%s#%d", st.RunID, st.StepIndex),
			Line: llm.Truncate(llm.Redact(fmt.Sprintf("run=%s 步骤%d %s kind=%s status=%d exit=%d 输出=%s 错误=%s",
				st.RunID, st.StepIndex, st.Name, st.Kind, st.Status, st.ExitCode, st.Output, st.ErrorMsg)), 500),
			Timestamp: time.Unix(st.Created, 0),
		})
		if len(hits) >= q.Limit {
			return hits, nil
		}
	}
	return hits, nil
}

var _ tools.LogSource = (*DBLogSource)(nil)

// MultiLogSource 组合多个日志数据源（admin 只用 DB；node 用 DB + 本机文件）。
//
// 语义：按顺序查询，一旦某个源抛错就**整体失败**——日志源出错时返回"看起来没命中"
// 的空结果最危险（模型会据此下"日志里没有相关记录"的结论），所以必须显式报错。
type MultiLogSource []tools.LogSource

// SearchLogs 实现 tools.LogSource。
func (m MultiLogSource) SearchLogs(ctx context.Context, q tools.LogQuery) ([]tools.LogHit, error) {
	hits := make([]tools.LogHit, 0, 16)
	for _, src := range m {
		if src == nil {
			continue
		}
		part, err := src.SearchLogs(ctx, q)
		if err != nil {
			return nil, err
		}
		hits = append(hits, part...)
		if q.Limit > 0 && len(hits) >= q.Limit {
			hits = hits[:q.Limit]
			break
		}
	}
	return hits, nil
}

var _ tools.LogSource = (MultiLogSource)(nil)

// ReadonlySQLSource 只读 SQL 数据源（mysql_query_readonly 工具的后端）。
//
// 三层防护，缺一不可：
//  1. 这里：语句必须是单条 SELECT/WITH，禁止分号多语句；
//  2. tools.SQLTool：语句守卫（拒绝写操作、强制 LIMIT、表名策略）；
//  3. 数据库账号：**必须**是只读账号（部署要求，工具层挡不住有写权限的账号）。
//
// 第 3 条是运维责任：本项目只保证前两条，文档里已明确写出这条前提。
type ReadonlySQLSource struct {
	// Timeout 单条查询超时（默认 8s）。
	Timeout time.Duration
	// MaxRows 最多返回行数（默认 200）。
	MaxRows int
}

// QueryReadonly 实现 tools.SQLSource。
func (s *ReadonlySQLSource) QueryReadonly(ctx context.Context, query string) ([]map[string]interface{}, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, fmt.Errorf("查询语句为空")
	}
	if db() == nil {
		return nil, fmt.Errorf("数据库未初始化")
	}
	// 去尾分号后不允许再出现分号：挡掉 "select 1; drop table x"
	trimmed := strings.TrimRight(q, "; \t\r\n")
	if strings.Contains(trimmed, ";") {
		return nil, fmt.Errorf("只允许单条查询语句（检测到多条语句）")
	}
	lower := strings.ToLower(trimmed)
	if !strings.HasPrefix(lower, "select") && !strings.HasPrefix(lower, "with") {
		return nil, fmt.Errorf("只允许只读查询（SELECT/WITH），收到：%s", llm.Truncate(q, 60))
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	maxRows := s.MaxRows
	if maxRows <= 0 {
		maxRows = 200
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	rows, err := db().WithContext(ctx).Raw(trimmed).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, 16)
	for rows.Next() {
		values := make([]interface{}, len(cols))
		ptrs := make([]interface{}, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		item := make(map[string]interface{}, len(cols))
		for i, c := range cols {
			switch v := values[i].(type) {
			case []byte:
				item[c] = llm.Truncate(string(v), 400)
			default:
				item[c] = v
			}
		}
		out = append(out, item)
		if len(out) >= maxRows {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

var _ tools.SQLSource = (*ReadonlySQLSource)(nil)
