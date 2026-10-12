package chaos

import "testing"

func TestGrade(t *testing.T) {
	attempts := []Attempt{
		{Fault: Fault{Kind: KindLatency}, Detected: true, Handled: true, Correct: true, DetectionMS: 100},
		{Fault: Fault{Kind: KindLatency}, Detected: true, Handled: true, Correct: false, DetectionMS: 200},
		{Fault: Fault{Kind: KindDiskFull}, Detected: true, Handled: false, Correct: false, DetectionMS: 300},
		{Fault: Fault{Kind: KindNodeDown}, Detected: false, Handled: false, Correct: false, DetectionMS: 0},
	}
	s := Grade(attempts)
	if s.Total != 4 || s.Correct != 1 {
		t.Fatalf("统计错误: %+v", s)
	}
	if s.License != 25 {
		t.Fatalf("驾照分应为 25，实际 %d", s.License)
	}
	if s.DetectRate != 0.75 {
		t.Fatalf("检测率应为 0.75，实际 %v", s.DetectRate)
	}
	if s.AvgDetectionMS != 150 {
		t.Fatalf("平均检测时延应为 150，实际 %d", s.AvgDetectionMS)
	}
	if s.ByKind["latency"] != 0.5 {
		t.Fatalf("latency 正确率应为 0.5，实际 %v", s.ByKind["latency"])
	}
}

func TestGradeNoSamples(t *testing.T) {
	s := Grade(nil)
	if s.License != -1 {
		t.Fatalf("无样本驾照分应为 -1，实际 %d", s.License)
	}
	if LicenseAtLeast(s, 60) {
		t.Fatalf("没考过不得算达标")
	}
}

func TestLicenseAtLeast(t *testing.T) {
	s := Grade([]Attempt{
		{Fault: Fault{Kind: KindLatency}, Detected: true, Handled: true, Correct: true},
		{Fault: Fault{Kind: KindLatency}, Detected: true, Handled: true, Correct: true},
		{Fault: Fault{Kind: KindLatency}, Detected: true, Handled: true, Correct: true},
	})
	if s.License != 100 || !LicenseAtLeast(s, 90) {
		t.Fatalf("全对驾照分应为 100，实际 %d", s.License)
	}
}

func TestCatalogIsControlled(t *testing.T) {
	cat := Catalog()
	if len(cat) == 0 {
		t.Fatalf("故障目录不能为空")
	}
	for _, f := range cat {
		if f.Kind == "" || f.Target == "" {
			t.Fatalf("故障必须声明 kind 与 target: %+v", f)
		}
		if f.Severity < 1 || f.Severity > 3 {
			t.Fatalf("severity 应在 1~3: %+v", f)
		}
	}
}
