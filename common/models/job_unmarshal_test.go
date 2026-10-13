package models

import "testing"

// 20 节点舰队实测暴露：未配置通知（notify_to 为空）的任务在自动分配时因
// Unmarshal() 报 "unexpected end of JSON input" 而被 continue 跳过，
// 于是**永远分配不出去**，且每次节点上下线都重复告警。
// 空列是合法状态（"没配通知""没用脚本"），不是错误。
func TestJobUnmarshalToleratesEmptyJSONColumns(t *testing.T) {
	job := &Job{}
	if err := job.Unmarshal(); err != nil {
		t.Fatalf("空 notify_to/script_id 不应报错，实际：%v", err)
	}
	if job.NotifyToArray != nil || job.ScriptIDArray != nil {
		t.Fatalf("空列不应产出非空切片：notify=%v script=%v", job.NotifyToArray, job.ScriptIDArray)
	}
}

func TestJobUnmarshalParsesValidColumns(t *testing.T) {
	job := &Job{NotifyTo: []byte("[1,2]"), ScriptID: []byte("[3]")}
	if err := job.Unmarshal(); err != nil {
		t.Fatalf("合法 JSON 不应报错：%v", err)
	}
	if len(job.NotifyToArray) != 2 || job.NotifyToArray[1] != 2 {
		t.Fatalf("NotifyToArray = %v, want [1 2]", job.NotifyToArray)
	}
	if len(job.ScriptIDArray) != 1 || job.ScriptIDArray[0] != 3 {
		t.Fatalf("ScriptIDArray = %v, want [3]", job.ScriptIDArray)
	}
}

// 容错只针对"空"，真正的脏数据仍必须报错——不能把错误吞掉。
func TestJobUnmarshalRejectsMalformedJSON(t *testing.T) {
	if err := (&Job{NotifyTo: []byte("{oops")}).Unmarshal(); err == nil {
		t.Fatal("notify_to 非法 JSON 应返回错误")
	}
	if err := (&Job{ScriptID: []byte("not-json")}).Unmarshal(); err == nil {
		t.Fatal("script_id 非法 JSON 应返回错误")
	}
}
