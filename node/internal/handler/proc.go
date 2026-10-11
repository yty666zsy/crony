package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coreos/etcd/clientv3"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/etcdclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"strconv"
	"strings"
	"sync/atomic"
)

// ErrConfigNotLoaded 配置尚未加载（进程启动异常的早期阶段）时，
// 不允许写执行记录，返回错误而不是 panic。
var ErrConfigNotLoaded = errors.New("config is not loaded")

// Information about the current task in execution
// key: /crony/proc/<node_uuid>/<job_id>/pid</job_id></node_uuid>
// value: indicates the start execution time
// The key expires automatically to prevent the key from being cleared after the process exits unexpectedly. The expiration time can be configured
type JobProc struct {
	*models.JobProc
}

func GetProcFromKey(key string) (proc *JobProc, err error) {
	ss := strings.Split(key, "/")
	var sslen = len(ss)
	if sslen < 5 {
		err = fmt.Errorf("invalid proc key [%s]", key)
		return
	}
	id, err := strconv.Atoi(ss[sslen-1])
	if err != nil {
		return
	}
	jobId, err := strconv.Atoi(ss[sslen-2])
	if err != nil {
		return
	}
	proc = &JobProc{
		JobProc: &models.JobProc{
			ID:       id,
			JobID:    jobId,
			NodeUUID: ss[sslen-3],
		},
	}
	return
}

func (p *JobProc) Key() string {
	return fmt.Sprintf(etcdclient.KeyEtcdProc, p.NodeUUID, p.JobID, p.ID)
}

func (p *JobProc) del() error {
	_, err := etcdclient.Delete(p.Key())
	return err
}

func (p *JobProc) Stop() {
	if p == nil {
		return
	}
	if !atomic.CompareAndSwapInt32(&p.Running, 1, 0) {
		return
	}
	p.Wg.Wait()

	if err := p.del(); err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("proc del[%s] err: %s", p.Key(), err.Error()))
	}
}

func WatchProc(nodeUUID string) clientv3.WatchChan {
	return etcdclient.Watch(fmt.Sprintf(etcdclient.KeyEtcdNodeProcProfile, nodeUUID), clientv3.WithPrefix())
}

func (p *JobProc) Start() error {
	if !atomic.CompareAndSwapInt32(&p.Running, 0, 1) {
		return nil
	}
	// 无论成功失败都 Done：旧实现只在成功路径调用 p.Wg.Done()，
	// 一旦写 etcd 失败，Running 会永远停在 1，随后 Stop() 里的 p.Wg.Wait() 会永久阻塞。
	p.Wg.Add(1)
	defer p.Wg.Done()

	b, err := json.Marshal(p.JobProcVal)
	if err != nil {
		atomic.StoreInt32(&p.Running, 0)
		return err
	}
	// 旧实现直接写 config.GetConfigModels().System.JobProcTtl：
	// 配置未加载时会空指针 panic（单元测试与启动早期失败分支都会命中）。
	cfg := config.GetConfigModels()
	if cfg == nil {
		atomic.StoreInt32(&p.Running, 0)
		return ErrConfigNotLoaded
	}
	_, err = etcdclient.PutWithTtl(p.Key(), string(b), cfg.System.JobProcTtl)
	if err != nil {
		atomic.StoreInt32(&p.Running, 0)
		return err
	}
	return nil
}
