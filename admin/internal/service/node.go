package service

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/tmnhs/crony/admin/internal/model/request"
	"github.com/tmnhs/crony/common/models"
	"github.com/tmnhs/crony/common/pkg/config"
	"github.com/tmnhs/crony/common/pkg/dbclient"
	"github.com/tmnhs/crony/common/pkg/etcdclient"
	"github.com/tmnhs/crony/common/pkg/logger"
	"github.com/tmnhs/crony/common/pkg/notify"
	"github.com/tmnhs/crony/common/pkg/utils"
	"strings"
	"sync"
	"time"
)

type NodeWatcherService struct {
	client   *etcdclient.Client
	nodeList map[string]models.Node
	lock     sync.Mutex
	// assignCh 是"分配未分配任务"的请求通道（容量 1 → 天然合并重复请求）。
	//
	// 为什么要合并：旧实现把 Sleep(5s)+分配 直接写在每个节点事件的处理路径里，
	// 20 个节点上线就是 20×5s 的串行阻塞（admin 启动时 extractNodes 同样中招）。
	// 现在事件只登记+投递请求，真正的等待与分配交给后台 worker 合并执行一次。
	assignCh chan struct{}
}

var DefaultNodeWatcher *NodeWatcherService

func NewNodeWatcherService() *NodeWatcherService {
	return &NodeWatcherService{
		client:   etcdclient.GetEtcdClient(),
		nodeList: make(map[string]models.Node),
		assignCh: make(chan struct{}, 1),
	}
}

// Watch 启动节点列表的初始同步与后续 watch。
//
// etcd 未初始化时必须返回可读错误，而不是解引用 null 客户端 panic：
// 真实启动测试里，etcd 不可用时 admin 直接以
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//	github.com/tmnhs/crony/admin/internal/service.(*NodeWatcherService).Watch
//
// 崩溃 —— 整个控制面连 /login 都起不来。note：控制面 HTTP API（含 Agent 诊断/订阅）
// 本身不依赖 etcd，所以这里的语义是"节点列表与调度协同降级不可用"，
// 由调用方决定是记录降级继续服务，还是 fail fast。
func (n *NodeWatcherService) Watch() error {
	if n == nil || n.client == nil {
		return fmt.Errorf("etcd 未初始化，节点列表与调度协同不可用（控制面 API 不受影响；请检查 etcd.endpoints 与 etcd 服务）")
	}
	resp, err := n.client.Get(context.Background(), etcdclient.KeyEtcdNodeProfile, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	_ = n.extractNodes(resp)

	go n.watcher()
	go n.runAssignWorker()
	return nil
}

func (n *NodeWatcherService) watcher() {
	if n == nil || n.client == nil {
		return
	}
	rch := n.client.Watch(context.Background(), etcdclient.KeyEtcdNodeProfile, clientv3.WithPrefix())
	for wresp := range rch {
		for _, ev := range wresp.Events {
			switch ev.Type {
			case mvccpb.PUT:
				n.setNodeList(n.GetUUID(string(ev.Kv.Key)), string(ev.Kv.Value))
			case mvccpb.DELETE:
				uuid := n.GetUUID(string(ev.Kv.Key))
				n.delNodeList(uuid)
				logger.GetLogger().Warn(fmt.Sprintf("crony node[%s] DELETE event detected", uuid))
				// 防御：DELETE 事件可能是**陈旧的**。节点重启时会先删自己的旧注册键、
				// 再写新键；旧租约过期与新注册也可能交错。watcher 收到的顺序不保证是最终状态，
				// 于是"先 DELETE 后 PUT"会让这条 DELETE 在节点已经复活之后才被处理。
				// 若此刻 etcd 里该 uuid 的节点键又存在，说明节点已经重新注册，这条事件已过期：
				// 此时做 FailOver / 删行会把一个**健康节点**上的任务白搬一次，
				// 并删掉它刚写入的库内行（造成"etcd 有、库里没有"的漂移）。
				if n.nodeKeyExists(uuid) {
					logger.GetLogger().Warn(fmt.Sprintf(
						"crony node[%s] DELETE event is stale（该节点键已重新出现，节点已重新注册），跳过 FailOver", uuid))
					continue
				}
				node := &models.Node{UUID: uuid}
				err := node.FindByUUID()
				if err != nil {
					// 不能 return：这里在 watcher 的事件循环内，return 会**永久结束节点监控**
					// （此后再也不处理上下线/FailOver）。库里查不到该节点只说明它没有落库行
					// （例如从未 Insert、或已被清理），跳过这一条事件即可。
					logger.GetLogger().Error(fmt.Sprintf("crony node[%s] find by uuid  error:%s", uuid, err.Error()))
					continue
				}

				success, fail, err := n.FailOver(uuid)
				if err != nil {
					// 同上：单次 FailOver 失败不应拖垮整个 watch 循环。
					logger.GetLogger().Error(fmt.Sprintf("crony node[%s] fail over error:%s", uuid, err.Error()))
					continue
				}
				// if the failover is all successful, delete the node in the database
				if fail.Count() == 0 {
					err = node.Delete()
					if err != nil {
						logger.GetLogger().Error(fmt.Sprintf("crony node[%s] delete by uuid  error:%s", uuid, err.Error()))
					}
				}
				//Node inactivation information defaults to email.
				msg := &notify.Message{
					Type:      notify.NotifyTypeMail,
					IP:        fmt.Sprintf("%s:%s", node.IP, node.PID),
					Subject:   "节点失活报警",
					Body:      fmt.Sprintf("[Crony Warning]crony node[%s] in the cluster has failed,，fail over success count:%d jobID are :%s ,fail count:%d jobID are :%s ", uuid, success.Count(), success.String(), fail.Count(), fail.String()),
					To:        config.GetConfigModels().Email.To,
					OccurTime: time.Now().Format(utils.TimeFormatSecond),
				}

				go notify.Send(msg)
			}
		}
	}
}

func (n *NodeWatcherService) extractNodes(resp *clientv3.GetResponse) []string {
	nodes := make([]string, 0)
	if resp == nil || resp.Kvs == nil {
		return nodes
	}
	for i := range resp.Kvs {
		if v := resp.Kvs[i].Value; v != nil {
			n.setNodeList(n.GetUUID(string(resp.Kvs[i].Key)), string(resp.Kvs[i].Value))
			nodes = append(nodes, string(v))
		}
	}
	return nodes
}

// nodeReadyDelay 新节点出现在 etcd 后，等它把 watch 建起来再推任务的时间。
//
// 为什么必须等：节点启动顺序是 Register(etcd PUT，admin 立刻可见) → Run()
// （先 loadJobs 做一次 Get，再 go watchJobs）。而节点侧的 WatchJobs 是**裸 watch**
// （无初始 Get、无 WithRev），admin 若在节点 watch 建立前推进任务，节点永远收不到。
// 这个 5s 就是给"节点把 watch 建起来"留的余量——它不能删，只能不阻塞别人。
const nodeReadyDelay = 5 * time.Second

// getNodeList 在锁内读取节点快照，避免与 watch work 协程的 setNodeList/delNodeList 并发读写 map。
func (n *NodeWatcherService) getNodeList(key string) (models.Node, bool) {
	n.lock.Lock()
	defer n.lock.Unlock()
	node, ok := n.nodeList[key]
	return node, ok
}

func (n *NodeWatcherService) setNodeList(key, val string) {
	var node models.Node
	err := json.Unmarshal([]byte(val), &node)
	if err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("discover node[%s] json error:%s", key, err.Error()))
		return
	}
	n.lock.Lock()
	n.nodeList[key] = node
	n.lock.Unlock()
	logger.GetLogger().Debug(fmt.Sprintf("discover node node[%s] with pid[%s]", key, val))
	// 立刻让节点对调度器可见（List2Array 立即可用），但**不在事件路径里 sleep**：
	// 只投递一次分配请求，由后台 worker 合并处理（F1）。
	n.requestAssign()
}

// requestAssign 非阻塞地请求一次"把未分配任务分出去"；多次请求合并成一次。
func (n *NodeWatcherService) requestAssign() {
	if n.assignCh == nil {
		return
	}
	select {
	case n.assignCh <- struct{}{}:
	default: // 已有待处理请求，无需再投
	}
}

// runAssignWorker 消费分配请求：等 nodeReadyDelay 后做一次全局分配。
//
// 旧实现是"每个节点事件各等 5s 各分配一遍"，20 节点即 20 次串行；合并成一次后
// 语义不变（仍然等节点就绪再加任务），但不再把 watcher 和 admin 启动路径拖住。
func (n *NodeWatcherService) runAssignWorker() {
	for range n.assignCh {
		time.Sleep(nodeReadyDelay)
		n.assignPendingJobs()
	}
}

// assignPendingJobs 把所有"未分配"且可解析的任务分配出去。
//
// 分配策略与 FailOver 统一：都走 AutoAllocateNodeFor（4-D 多目标加权打分，
// 打分选不出才回退旧策略"任务数最少"）。
//
// 为什么必须统一（20 节点舰队实测，F3）：此前这条路径用的是裸 AutoAllocateNode，
// 于是**同一批任务**会因"是新节点接入分配"还是"某节点失联迁移"而落到不同策略上——
// 带 node_selector/标签硬约束的任务在新节点接入时**根本不被过滤**，
// 可能被派给一个装不了它的机器。统一口径后，两条路径的选点理由一致、可解释。
//
// 其它保持旧行为：只处理"未分配"的任务，逐个分配，失败只跳过该任务。
func (n *NodeWatcherService) assignPendingJobs() {
	jobs, err := DefaultJobService.GetNotAssignedJob()
	if err != nil {
		logger.GetLogger().Warn(fmt.Sprintf("get not assigned job err:%s", err.Error()))
		return
	}
	for _, job := range jobs {
		if err = job.Unmarshal(); err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("assign unassigned job[%d] 解析失败:%s", job.ID, err.Error()))
			continue
		}
		oldUUID := job.RunOn
		nodeUUID := DefaultJobService.AutoAllocateNodeFor(&job)
		if nodeUUID == "" && DefaultSchedulerService.HasHardRequirement(&job) {
			// 硬约束（标签/能力）当前一个节点都满足不了：**保持未分配**。
			// 下面的 anyNodeUUID 兜底绝不能在这种情况下生效——那等于绕过硬约束，
			// 把任务派到一台装不了它的机器上（F3 的同一类错误）。
			logger.GetLogger().Warn(fmt.Sprintf(
				"job[%d] 声明了硬约束但当前没有满足条件的节点，保持未分配（避免派给装不了它的机器）", job.ID))
			continue
		}
		if nodeUUID == "" {
			// 自动分配选不出（例如所有在册节点都被判定失联）时，退化为"随便挑一个已在册节点"，
			// 与旧实现"直接分给触发事件的那个节点"是同一意图。
			nodeUUID = n.anyNodeUUID()
		}
		if nodeUUID == "" {
			return // 没有任何可用节点，等下一次节点事件
		}
		if err = n.assignJob(nodeUUID, &job); err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("assign unassigned job[%d]  error:%s", job.ID, err.Error()))
			continue
		}
		// 迁移成功后删掉旧节点的 etcd key
		if _, err = etcdclient.Delete(fmt.Sprintf(etcdclient.KeyEtcdJob, oldUUID, job.ID)); err != nil {
			logger.GetLogger().Error(fmt.Sprintf("node[%s] job[%d] fail over etcd delete job error:%s", nodeUUID, job.ID, err.Error()))
			continue
		}
	}
}

// anyNodeUUID 返回已在册节点中的任意一个（用于分配兜底）。
func (n *NodeWatcherService) anyNodeUUID() string {
	n.lock.Lock()
	defer n.lock.Unlock()
	for k := range n.nodeList {
		return k
	}
	return ""
}

// nodeKeyExists 查 etcd 里该 uuid 的节点注册键当前是否还在。
//
// 用于识别**陈旧的 DELETE 事件**：节点重启（先删旧键、再写新键）或"旧租约过期"
// 与"新注册"交错时，watcher 可能先收到 DELETE、此时节点其实已经复活。
// 客户端不可用时返回 false（保持旧行为：按真的失联处理）。
func (n *NodeWatcherService) nodeKeyExists(uuid string) bool {
	if n == nil || n.client == nil || uuid == "" {
		return false
	}
	resp, err := n.client.Get(context.Background(), fmt.Sprintf(etcdclient.KeyEtcdNode, uuid))
	if err != nil {
		return false
	}
	return len(resp.Kvs) > 0
}

func (n *NodeWatcherService) delNodeList(key string) {
	n.lock.Lock()
	defer n.lock.Unlock()
	delete(n.nodeList, key)
	logger.GetLogger().Debug(fmt.Sprintf("delelte node[%s]", key))
}

func (n *NodeWatcherService) List2Array() []string {
	n.lock.Lock()
	defer n.lock.Unlock()
	nodes := make([]string, 0)

	for k, _ := range n.nodeList {
		nodes = append(nodes, k)
	}
	return nodes
}

func (n *NodeWatcherService) Close() error {
	return nil
}

func (n *NodeWatcherService) GetUUID(key string) string {
	// /crony/node/<node_uuid>
	index := strings.LastIndex(key, "/")
	if index == -1 {
		return ""
	}
	return key[index+1:]
}

func (n *NodeWatcherService) Search(s *request.ReqNodeSearch) ([]models.Node, int64, error) {
	db := dbclient.GetMysqlDB().Table(models.CronyNodeTableName)
	if len(s.UUID) > 0 {
		db = db.Where("uuid = ?", s.UUID)
	}
	if len(s.IP) > 0 {
		db = db.Where("ip = ?", s.IP)
	}
	if s.Status > 0 {
		db = db.Where("status = ?", s.Status)
	}
	if s.UpTime > 0 {
		db = db.Where("up > ?", s.UpTime)
	}
	// 必须是 make([]T, 0)：旧代码 make([]T, 2) 会让 GORM 在既有 2 个零值元素后再追加，
	// 导致分页结果里混入空行、total 与列表长度不一致。
	nodes := make([]models.Node, 0)
	var total int64
	err := db.Count(&total).Error
	if err != nil {
		return nil, 0, err
	}
	err = db.Limit(s.PageSize).Offset((s.Page - 1) * s.PageSize).Order("up desc").Find(&nodes).Error
	if err != nil {
		return nil, 0, err
	}

	return nodes, total, nil
}

func (n *NodeWatcherService) GetJobCount(nodeUUID string) (int, error) {
	resps, err := etcdclient.Get(fmt.Sprintf(etcdclient.KeyEtcdJobProfile, nodeUUID), clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		return 0, err
	}
	return int(resps.Count), nil
}

type Result []int

func (r Result) Count() (count int) {
	for _, v := range r {
		if v != 0 {
			count++
		}
	}
	return
}
func (r Result) String() (str string) {
	str = "["
	for _, v := range r {
		if v != 0 {
			str += fmt.Sprintf("%d,", v)
		}
	}
	str += "]"
	return
}

func (n *NodeWatcherService) assignJob(nodeUUID string, job *models.Job) (err error) {
	if nodeUUID == "" {
		return fmt.Errorf("node uuid can't be null")
	}
	node, ok := n.getNodeList(nodeUUID)
	if !ok {
		return fmt.Errorf("assign unassigned job[%d] but  node[%s] not exist ", job.ID, nodeUUID)
	}
	job.InitNodeInfo(models.JobStatusAssigned, node.UUID, node.Hostname, node.IP)

	b, err := json.Marshal(job)
	if err != nil {
		return
	}
	_, err = etcdclient.Put(fmt.Sprintf(etcdclient.KeyEtcdJob, nodeUUID, job.ID), string(b))
	if err != nil {
		return
	}
	err = job.Update()
	if err != nil {
		return
	}
	return
}

func (n *NodeWatcherService) FailOver(nodeUUID string) (success Result, fail Result, err error) {
	jobs, err := n.GetJobs(nodeUUID)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("node[%s] fail over get jobs error:%s", nodeUUID, err.Error()))
		return
	}
	if len(jobs) == 0 {
		return
	}
	for _, job := range jobs {
		/*//Determine whether shell command failover is supported
		if job.Type == models.JobTypeCmd && !config.GetConfigModels().System.CmdAutoAllocation {
			logger.GetLogger().Warn(fmt.Sprintf("node[%s] job[%d] fail over don't support cmd type", nodeUUID, job.ID))
			fail = append(fail, job.ID)
			continue
		}*/
		oldUUID := job.RunOn
		// 阶段 4-D：优先多目标加权打分（考虑负载/容量/亲和/健康度与任务硬约束），
		// 打分选不出时回退旧策略（任务数最少）。见 job.go AutoAllocateNodeFor。
		autoUUID := DefaultJobService.AutoAllocateNodeFor(&job)
		if autoUUID == "" {
			logger.GetLogger().Warn(fmt.Sprintf("node[%s] job[%d] fail over auto allocate node error", nodeUUID, job.ID))
			fail = append(fail, job.ID)
			continue
		}
		err = n.assignJob(autoUUID, &job)
		if err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("node[%s] job[%d] fail over assign job error", nodeUUID, job.ID))
			fail = append(fail, job.ID)
			continue
		}
		//Delete the key value if the transfer is successful
		_, err = etcdclient.Delete(fmt.Sprintf(etcdclient.KeyEtcdJob, oldUUID, job.ID))
		if err != nil {
			logger.GetLogger().Error(fmt.Sprintf("node[%s] job[%d] fail over etcd delete job error:%s", nodeUUID, job.ID, err.Error()))
			fail = append(fail, job.ID)
			continue
		}
		success = append(success, job.ID)
	}
	return
}

// get all the job under a node
func (n *NodeWatcherService) GetJobs(nodeUUID string) (jobs []models.Job, err error) {
	resps, err := etcdclient.Get(fmt.Sprintf(etcdclient.KeyEtcdJobProfile, nodeUUID), clientv3.WithPrefix())
	if err != nil {
		return
	}
	count := len(resps.Kvs)
	if count == 0 {
		return
	}
	for _, j := range resps.Kvs {
		var job models.Job
		if err := json.Unmarshal(j.Value, &job); err != nil {
			logger.GetLogger().Warn(fmt.Sprintf("job[%s] umarshal err: %s", string(j.Key), err.Error()))
			continue
		}
		jobs = append(jobs, job)
	}
	return
}

func (n *NodeWatcherService) GetNodeCount(status int) (int64, error) {
	db := dbclient.GetMysqlDB().Table(models.CronyNodeTableName)
	if status > 0 {
		db = db.Where("status = ?", status)
	}
	var total int64
	err := db.Count(&total).Error
	if err != nil {
		return 0, err
	}
	return total, nil
}

func GetNodeSystemInfo(uuid string) (s *utils.Server, err error) {
	defer func() {
		_, err = etcdclient.Delete(fmt.Sprintf(etcdclient.KeyEtcdSystemSwitch, uuid))
	}()
	s = new(utils.Server)
	res, err := etcdclient.Get(fmt.Sprintf(etcdclient.KeyEtcdSystemGet, uuid), clientv3.WithPrefix())
	if err != nil || len(res.Kvs) == 0 {
		return
	}
	err = json.Unmarshal(res.Kvs[0].Value, s)
	if err != nil {
		logger.GetLogger().Error(fmt.Sprintf("json error:%v", err))
	}
	return
}
