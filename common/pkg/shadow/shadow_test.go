package shadow

import "testing"

func base() Observation {
	return Observation{
		ExitCode: 0, StdoutHash: "aaa", StderrHash: "bbb",
		SideEffects: []SideEffect{
			{Kind: "file", Key: "/data/out.csv", Change: "modified"},
			{Kind: "dbrow", Key: "orders:42", Change: "updated"},
		},
	}
}

func TestReconcileIdentical(t *testing.T) {
	d := Reconcile(base(), base())
	if !d.Same || d.Unexpected {
		t.Fatalf("完全一致应 Same=true Unexpected=false，实际 %+v", d)
	}
	if !AllowPromotion(d) {
		t.Fatalf("零差异应允许放量")
	}
}

func TestReconcileExitCodeDiff(t *testing.T) {
	c := base()
	c.ExitCode = 1
	d := Reconcile(base(), c)
	if d.Same || !d.Unexpected {
		t.Fatalf("退出码不同应判意外差异，实际 %+v", d)
	}
	if AllowPromotion(d) {
		t.Fatalf("有差异不得放量")
	}
}

func TestReconcileExtraSideEffect(t *testing.T) {
	c := base()
	c.SideEffects = append(c.SideEffects, SideEffect{Kind: "process", Key: "nginx", Change: "stopped"})
	d := Reconcile(base(), c)
	if d.Same {
		t.Fatalf("多出副作用必须判差异")
	}
	foundExtra := false
	for _, s := range d.Differences {
		if contains(s, "影子多出副作用") {
			foundExtra = true
		}
	}
	if !foundExtra {
		t.Fatalf("差异说明应点出多出的副作用，实际 %v", d.Differences)
	}
}

func TestReconcileMissingSideEffect(t *testing.T) {
	c := base()
	c.SideEffects = c.SideEffects[:1] // 少一条
	d := Reconcile(base(), c)
	if d.Same {
		t.Fatalf("少一条副作用必须判差异")
	}
}

func TestReconcileStdoutDiff(t *testing.T) {
	c := base()
	c.StdoutHash = "zzz"
	if Reconcile(base(), c).Same {
		t.Fatalf("stdout 不同必须判差异")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
