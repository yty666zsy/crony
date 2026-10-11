<script setup>
import { computed, nextTick, onMounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { searchNodes } from '@/api/node'
import { formatRelative, formatSeconds } from '@/utils/format'

/**
 * /terminal —— 交互式终端。
 *
 * ⚠️ 阶段 2 交付项：真实终端需要 admin 侧做 WebSocket/SSE 的 PTY 代理
 *   （把 crony-node 上的 shell 会话双向转发），后端目前完全没有这条链路，
 *   POST /node/terminal 也是规划中接口。
 *   出于安全，本页**不会**尝试连接任何真实主机，只在本地回显一个受限的
 *   命令白名单模拟器，用于固定交互形态（多标签、节点上下文、命令历史、审计关联）。
 */

const nodes = ref([])
const selected = ref('')
const input = ref('')
const logRef = ref(null)

const tabs = ref([
  { id: 't1', title: 'node-prod-01 · bash', nodeUuid: '', closed: false },
])
const activeTab = ref('t1')

/** 每个标签独立的输出缓冲：{ [tabId]: [{ type, text }] } */
const buffers = reactive({ t1: [] })
const histories = reactive({ t1: [] })
const historyIndex = reactive({ t1: -1 })

const WHITELIST = ['help', 'ls', 'pwd', 'whoami', 'uptime', 'date', 'df -h', 'free -m', 'ps aux', 'clear']

const nodeInfo = computed(() => nodes.value.find((n) => n.uuid === selected.value) || null)

function push(tabId, type, text) {
  if (!buffers[tabId]) buffers[tabId] = []
  buffers[tabId].push({ type, text, ts: Date.now() })
}

function welcome(tabId, node) {
  push(tabId, 'system', `# crony console (Mock) — 已选择节点 ${node?.hostname || '未选择'}`)
  push(tabId, 'system', '# 真实 PTY 代理（WebSocket）为阶段 2 交付项，当前为本地白名单模拟器')
  push(tabId, 'system', `# 支持的命令：${WHITELIST.join(' / ')}`)
  push(tabId, 'system', '')
}

function openTab() {
  const id = `t${Date.now()}`
  const node = nodeInfo.value
  tabs.value.push({
    id,
    title: `${node ? node.hostname : '未选择节点'} · bash`,
    nodeUuid: selected.value,
    closed: false,
  })
  buffers[id] = []
  histories[id] = []
  historyIndex[id] = -1
  activeTab.value = id
  welcome(id, node)
}

function closeTab(id) {
  const idx = tabs.value.findIndex((t) => t.id === id)
  if (idx === -1) return
  tabs.value.splice(idx, 1)
  delete buffers[id]
  if (tabs.value.length === 0) {
    openTab()
    return
  }
  if (activeTab.value === id) {
    activeTab.value = tabs.value[Math.max(0, idx - 1)].id
  }
}

async function scrollToBottom() {
  await nextTick()
  const el = logRef.value
  if (el) el.scrollTop = el.scrollHeight
}

/** 白名单模拟器：只回显假数据，绝不产生真实副作用 */
function simulate(cmd) {
  const node = nodeInfo.value || { hostname: 'unknown', ip: '0.0.0.0' }
  const now = new Date()
  const base = cmd.trim().split(/\s+/)[0]
  switch (base) {
    case 'help':
      return `可用命令：\n  ${WHITELIST.join('\n  ')}\n其他命令会被拒绝（阶段 2 才会转发到真实 shell）。`
    case 'ls':
      return 'bin  boot  data  dev  etc  home  opt  srv  var'
    case 'pwd':
      return '/root'
    case 'whoami':
      return 'root (crony-node 执行身份)'
    case 'uptime':
      return ` ${now.toTimeString().slice(0, 8)} up 42 days,  3:11,  2 users,  load average: 0.42, 0.38, 0.35`
    case 'date':
      return now.toString()
    case 'df':
      return [
        'Filesystem      Size  Used Avail Use% Mounted on',
        '/dev/vda1        99G   43G   51G  46% /',
        '/dev/vdb1       493G  425G   43G  91% /data   <-- 触发告警 AL-501',
        'tmpfs           7.9G     0  7.9G   0% /dev/shm',
      ].join('\n')
    case 'free':
      return [
        '              total        used        free      shared  buff/cache   available',
        'Mem:           15Gi       9.4Gi       1.1Gi       212Mi       5.1Gi       5.4Gi',
        'Swap:         4.0Gi       128Mi       3.9Gi',
      ].join('\n')
    case 'ps':
      return [
        'USER       PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND',
        'root         1  0.0  0.1 168420 11204 ?        Ss   Sep12   1:22 /sbin/init',
        'root       912  2.4  3.1 2451230 512340 ?      Ssl  Sep12  88:12 crony-node --conf /etc/crony/node.ini',
        'root      1455  0.7  1.2 1210044 198220 ?      Ssl  Sep12  24:03 mysqld',
      ].join('\n')
    default:
      return `bash: ${cmd}: 命令不在阶段 1 白名单内（仅在 Mock 终端被拒绝，不会转发到 ${node.hostname}）`
  }
}

async function run() {
  const tabId = activeTab.value
  const cmd = input.value
  if (!cmd.trim()) return

  push(tabId, 'input', `${nodeInfo.value?.hostname || 'unknown'}:~# ${cmd}`)
  histories[tabId] = histories[tabId] || []
  histories[tabId].push(cmd)
  historyIndex[tabId] = histories[tabId].length
  input.value = ''

  if (cmd.trim() === 'clear') {
    buffers[tabId] = []
    return
  }

  if (!WHITELIST.some((c) => cmd.trim() === c || cmd.trim().startsWith(`${c} `))) {
    push(tabId, 'error', `权限拒绝：命令「${cmd.trim()}」不在只读白名单内。`)
    push(tabId, 'system', '阶段 1 控制台只允许只读命令；写操作请通过定时任务 + 审计流程执行。')
    scrollToBottom()
    return
  }

  await new Promise((r) => setTimeout(r, 160)) // 模拟回包延迟
  push(tabId, 'output', simulate(cmd))
  push(tabId, 'system', '')
  scrollToBottom()
}

function historyUp() {
  const list = histories[activeTab.value] || []
  if (!list.length) return
  const idx = historyIndex[activeTab.value]
  const next = idx > 0 ? idx - 1 : 0
  historyIndex[activeTab.value] = next
  input.value = list[next] || ''
}

function historyDown() {
  const list = histories[activeTab.value] || []
  const idx = historyIndex[activeTab.value]
  if (idx < list.length - 1) {
    historyIndex[activeTab.value] = idx + 1
    input.value = list[idx + 1]
  } else {
    historyIndex[activeTab.value] = list.length
    input.value = ''
  }
}

function attachNode() {
  const tab = tabs.value.find((t) => t.id === activeTab.value)
  if (!tab) return
  const node = nodeInfo.value
  tab.nodeUuid = selected.value
  tab.title = `${node ? node.hostname : '未选择节点'} · bash`
  push(tab.id, 'system', `# 会话目标已切换为 ${node ? `${node.hostname} (${node.ip})` : '未选择'}`)
  scrollToBottom()
}

onMounted(async () => {
  const res = await searchNodes({ page: 1, page_size: 50 })
  nodes.value = res?.list || []
  const online = nodes.value.find((n) => n.status === 1)
  if (online) {
    selected.value = online.uuid
    const tab = tabs.value[0]
    tab.nodeUuid = online.uuid
    tab.title = `${online.hostname} · bash`
  }
  welcome('t1', nodeInfo.value)
  ElMessage.info('终端为 Mock 模拟器：真实 PTY 代理属于阶段 2 交付项')
})
</script>

<template>
  <div class="space-y-4">
    <!-- 顶部：会话上下文 -->
    <section class="card card-pad">
      <div class="flex flex-wrap items-end gap-3">
        <div class="min-w-[240px] flex-1">
          <p class="section-label mb-1.5">目标节点</p>
          <el-select v-model="selected" placeholder="选择 crony-node 节点" filterable class="w-full">
            <el-option
              v-for="n in nodes"
              :key="n.uuid"
              :label="`${n.hostname} · ${n.ip}${n.status === 2 ? '（失联，禁止连接）' : ''}`"
              :value="n.uuid"
              :disabled="n.status === 2"
            />
          </el-select>
        </div>

        <div v-if="nodeInfo" class="grid flex-1 grid-cols-2 gap-x-6 gap-y-1 sm:grid-cols-4">
          <div>
            <p class="text-[10px] text-ink-dim">状态</p>
            <p class="text-[11px]" :class="nodeInfo.status === 1 ? 'text-ok' : 'text-danger'">
              {{ nodeInfo.status === 1 ? '在线' : '失联' }}
            </p>
          </div>
          <div>
            <p class="text-[10px] text-ink-dim">版本</p>
            <p class="text-[11px] text-ink-muted">{{ nodeInfo.version }}</p>
          </div>
          <div>
            <p class="text-[10px] text-ink-dim">PID</p>
            <p class="font-mono text-[11px] text-ink-muted">{{ nodeInfo.pid }}</p>
          </div>
          <div>
            <p class="text-[10px] text-ink-dim">最后心跳</p>
            <p class="text-[11px] text-ink-muted">
              {{ formatRelative(nodeInfo.status === 1 ? nodeInfo.up : nodeInfo.down) }}
            </p>
          </div>
        </div>

        <div class="flex items-center gap-2">
          <el-button @click="attachNode">绑定到当前标签</el-button>
          <el-button type="primary" plain @click="openTab">
            <el-icon class="mr-1"><Plus /></el-icon>新建会话
          </el-button>
        </div>
      </div>

      <el-alert
        type="warning"
        :closable="false"
        show-icon
        class="mt-3"
        title="阶段 2 功能预览：本页不会连接真实主机"
        description="真实终端需要 admin 提供 WebSocket PTY 代理（POST /node/terminal 规划中）。当前为本地白名单只读模拟器，仅用于固定交互形态，不会产生任何真实副作用。"
      />
    </section>

    <!-- 终端窗口 -->
    <section class="card overflow-hidden">
      <!-- 标签栏 -->
      <div class="flex items-center gap-1 border-b border-navy-600 bg-navy-850 px-2 py-1.5">
        <div
          v-for="t in tabs"
          :key="t.id"
          class="group flex cursor-pointer items-center gap-2 rounded-md px-3 py-1.5 text-[11px] transition-colors"
          :class="activeTab === t.id ? 'bg-navy-700 text-ink' : 'text-ink-dim hover:bg-navy-800'"
          @click="activeTab = t.id"
        >
          <el-icon :size="12" :class="t.nodeUuid ? 'text-ok' : 'text-ink-dim'"><Monitor /></el-icon>
          <span class="max-w-[220px] truncate">{{ t.title }}</span>
          <button
            v-if="tabs.length > 1"
            class="opacity-0 transition-opacity group-hover:opacity-100"
            @click.stop="closeTab(t.id)"
          >
            <el-icon :size="11"><Close /></el-icon>
          </button>
        </div>
        <button
          class="ml-1 flex h-6 w-6 items-center justify-center rounded text-ink-dim hover:bg-navy-700 hover:text-ink"
          @click="openTab"
        >
          <el-icon :size="13"><Plus /></el-icon>
        </button>

        <div class="ml-auto flex items-center gap-2 pr-1">
          <span class="chip border-warn/40 text-warn">Mock</span>
          <span v-if="nodeInfo" class="chip">
            运行 {{ formatSeconds(Math.floor(Date.now() / 1000) - nodeInfo.up) }}
          </span>
        </div>
      </div>

      <!-- 输出区 -->
      <div
        ref="logRef"
        class="scroll-area h-[420px] overflow-auto bg-[#070b16] px-4 py-3 font-mono text-[12px] leading-6"
      >
        <template v-for="tab in tabs" :key="tab.id">
          <div v-show="activeTab === tab.id">
            <p
              v-for="(line, i) in buffers[tab.id] || []"
              :key="i"
              class="whitespace-pre-wrap break-words"
              :class="{
                'text-ok': line.type === 'input',
                'text-ink-dim': line.type === 'system',
                'text-danger': line.type === 'error',
                'text-[#c9d4ea]': line.type === 'output',
              }"
            >
              {{ line.text }}
            </p>
            <p class="text-ok">
              {{ nodeInfo?.hostname || 'unknown' }}:~# <span class="animate-pulse">▋</span>
            </p>
          </div>
        </template>
      </div>

      <!-- 输入区 -->
      <div class="flex items-center gap-2 border-t border-navy-600 bg-navy-850 px-3 py-2.5">
        <span class="shrink-0 font-mono text-[12px] text-ok">
          {{ nodeInfo?.hostname || 'unknown' }}:~#
        </span>
        <input
          v-model="input"
          class="flex-1 bg-transparent font-mono text-[12px] text-ink outline-none placeholder:text-ink-dim"
          placeholder="输入命令（↑/↓ 调历史，Enter 执行，支持 help）"
          @keyup.enter="run"
          @keyup.up.prevent="historyUp"
          @keyup.down.prevent="historyDown"
        />
        <el-button size="small" type="primary" plain @click="run">执行</el-button>
      </div>
    </section>

    <div class="grid gap-4 lg:grid-cols-2">
      <div class="card card-pad">
        <p class="card-title">为什么现在只做只读？</p>
        <ul class="mt-2 space-y-1.5 text-[11px] leading-5 text-ink-dim">
          <li>· 直连 shell 是最危险的能力：一旦前端被 XSS 或越权访问，等于拿到全部节点的 root。</li>
          <li>· 阶段 1 的定位是"先让所有变更走定时任务 + 审计"，因此只放行只读命令。</li>
          <li>· 阶段 2 计划：PTY 代理 + 会话录制 + 命令级审批（L2），并把每一次输入写入 audit_event。</li>
        </ul>
      </div>
      <div class="card card-pad">
        <p class="card-title">目标链路（阶段 2 规划）</p>
        <pre class="mt-2 overflow-x-auto rounded-lg bg-navy-900 p-3 font-mono text-[10px] leading-5 text-ink-muted">Browser ──WS──> admin /node/terminal
                   │  (JWT + RequireAdmin + 会话审计)
                   └──> node agent ──pty──> /bin/bash
                        ↑ 输出流式回传，支持 resize / 中断</pre>
        <p class="mt-2 text-[11px] text-ink-dim">
          后端 router.go 当前没有该路由，模型里也没有 terminal session 表。
        </p>
      </div>
    </div>
  </div>
</template>