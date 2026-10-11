// Package acceptance 是 crony AI-Agent 改造的**黑盒案例验收**程序。
//
// 它只通过 HTTP + 只读 SQL 观察系统行为，不 import 任何内部包、不改动被测代码，
// 因此可以对着"任意一版已部署的 admin"跑：这正是验收该有的位置。
//
// 运行方式、环境变量、用例目录见同目录 README.md；门控 tag 为 acceptance。
package acceptance
