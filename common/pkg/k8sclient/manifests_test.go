package k8sclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	yamlutil "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	sigsyaml "sigs.k8s.io/yaml"
)

// 阶段 K4：`deploy/k8s/*.yaml` 的**离线**校验。
//
// 为什么要有这个测试：真集群验证一次是"最终手段"，不是"唯一手段"。
// `kubectl apply --dry-run=client` 在没有集群时也会去连 apiserver 做 discovery
// （本机实测：`the server has asked for the client to provide credentials`），
// 所以它替代不了离线校验。这里改用 client-go 自带的 scheme 把每个文档
// **严格**解析成强类型对象：
//   - 字段名/类型写错（例如 resources 写成字符串）→ 直接失败；
//   - kind 拼错（scheme 里没有这个 kind）→ 直接失败；
//   - 缺 metadata.name → 直接失败。
//
// 这个测试**不能**替代的：RBAC 是否够用、调度能否落地、镜像能否拉起。
// 那些只能真集群跑，验收报告里如实标注为 SKIP。
func TestDeployManifestsDecodeStrictly(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "deploy", "k8s")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 manifests 目录失败: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("装配 scheme 失败: %v", err)
	}

	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		// kind 集群配置不是 K8s 资源（`kind: Cluster` 属于 kind 自己的 API）：
		// 显式跳过并写清理由——不写成"scheme 里没注册就跳过"，
		// 那会让真正的 kind 拼写错误静默通过。
		if name == "kind-cluster.yaml" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		dec := yamlutil.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
		docIndex := 0
		for {
			var doc map[string]interface{}
			err := dec.Decode(&doc)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s 第 %d 个文档 YAML 解析失败: %v", name, docIndex+1, err)
			}
			if len(doc) == 0 {
				continue
			}
			docIndex++
			where := fmt.Sprintf("%s 第 %d 个文档", name, docIndex)

			apiVersion, _ := doc["apiVersion"].(string)
			kind, _ := doc["kind"].(string)
			if apiVersion == "" || kind == "" {
				t.Fatalf("%s 缺 apiVersion/kind", where)
			}
			gvk := schema.FromAPIVersionAndKind(apiVersion, kind)
			obj, err := scheme.New(gvk)
			if err != nil {
				t.Fatalf("%s 的 %s/%s 不在 client-go scheme 里（kind 拼错了？）: %v", where, apiVersion, kind, err)
			}
			encoded, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("%s 转 JSON 失败: %v", where, err)
			}
			// UnmarshalStrict：未知字段同样报错，这是"清单字段写错但 apply 成功"的第一道防线
			if err := sigsyaml.UnmarshalStrict(encoded, obj); err != nil {
				t.Fatalf("%s 严格解析为 %s 失败: %v", where, kind, err)
			}
			accessor, err := meta.Accessor(obj)
			if err != nil {
				t.Fatalf("%s 取 metadata 失败: %v", where, err)
			}
			if accessor.GetName() == "" {
				t.Fatalf("%s 缺 metadata.name（apply 后无法寻址）", where)
			}
			checked++
		}
	}
	// 断言"确实校验到了东西"：文件被误删/改名让循环空转，是这类测试最典型的假绿。
	if checked < 8 {
		t.Fatalf("只校验了 %d 个文档，少于预期（manifests 被删了？）", checked)
	}
	t.Logf("已严格校验 %d 个 K8s 文档", checked)
}

// TestStrictDecodeRejectsTypos 证明上面那条测试**不是假绿**：
// 同样的解析路径喂进去一个字段拼错的文档、一个不存在的 kind，都必须报错。
//
// 没有这个用例，"严格解析"只是注释里的一句承诺——而这类"防御性断言其实没生效"
// 正是最容易被当成安全网的东西。
func TestStrictDecodeRejectsTypos(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("装配 scheme 失败: %v", err)
	}

	decode := func(docJSON string) error {
		var doc map[string]interface{}
		if err := json.Unmarshal([]byte(docJSON), &doc); err != nil {
			t.Fatalf("测试输入非法 JSON: %v", err)
		}
		apiVersion, _ := doc["apiVersion"].(string)
		kind, _ := doc["kind"].(string)
		obj, err := scheme.New(schema.FromAPIVersionAndKind(apiVersion, kind))
		if err != nil {
			return err
		}
		return sigsyaml.UnmarshalStrict([]byte(docJSON), obj)
	}

	// 1) 字段拼错：spec.template.spec.containers 写成 contianers
	err := decode(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"x"},
		"spec":{"selector":{"matchLabels":{"app":"x"}},
		"template":{"metadata":{"labels":{"app":"x"}},
		"spec":{"contianers":[{"name":"c","image":"i"}]}}}}`)
	if err == nil {
		t.Fatal("字段拼错必须报错（否则 manifests 只能靠真集群才发现问题）")
	}

	// 2) 类型写错：replicas 写成字符串
	err = decode(`{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"x"},"spec":{"replicas":"三个"}}`)
	if err == nil {
		t.Fatal("类型写错必须报错")
	}

	// 3) kind 不存在
	err = decode(`{"apiVersion":"apps/v1","kind":"Deploymnet","metadata":{"name":"x"}}`)
	if err == nil {
		t.Fatal("kind 拼错必须报错")
	}
}
