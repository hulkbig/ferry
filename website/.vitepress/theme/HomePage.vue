<script setup>
import { computed, ref } from 'vue'
import { withBase } from 'vitepress'
import FlowCard from './FlowCard.vue'
const props = defineProps({ language: { type: String, default: 'zh' } })
const en = computed(() => props.language === 'en')
const link = path => withBase((en.value ? '/en' : '') + path + (path.endsWith('/') ? '' : '.html'))
const copyState = ref('')
const command = 'curl -fsSLo compose.yaml https://github.com/hulkbig/ferry/releases/download/v0.1.0/compose.yaml\ndocker compose up -d'
async function copy() { try { await navigator.clipboard.writeText(command); copyState.value = en.value ? 'Copied' : '已复制' } catch { copyState.value = en.value ? 'Select and copy the commands' : '请选中命令复制' } }
const flows = computed(() => en.value ? [
  ['request', 'Send a request. Pick a route.', 'Different clients enter through Ferry. Each request goes to an eligible upstream.'],
  ['response', 'Return the result. Keep working.', 'Streamed text, tool arguments and completion events travel back to the same client.'],
  ['fallback', 'Primary fails. A fallback takes over.', 'A temporary failure on one account can move the request to a compatible backup.'],
  ['queue', 'Rate limited? Wait, then continue.', 'Hold requests while capacity is unavailable; release them in order when it opens.'],
  ['logical', 'Change models. Keep your integration.', 'Your app calls a logical model such as work. Change the underlying model, account or route in Ferry without changing client configuration. Hermes is one example.', '/guide/logical-models', 'Configure logical models'],
  ['autoreset', 'Redeem credits before they expire.', 'AutoReset redeems available Codex reset credits within the expiry threshold and records the outcome. Default: 20 minutes before expiry; can be disabled.', '/guide/autoreset', 'Configure AutoReset'],
] : [
  ['request', '请求发出，选好线路', 'Claude Code、Hermes、Codex 发来请求，Ferry 按路由和能力选择上游。'],
  ['response', '结果返回，继续执行', '文本、工具参数和完成事件，通过同一个入口回到客户端。'],
  ['fallback', '首选故障，备用接力', '首选账号临时失败，换一条兼容线路继续，不必手动重发。'],
  ['queue', '遇到限流，排队再走', '容量不足先等待，恢复后按序放行；不让突发请求挤向上游。'],
  ['logical', '模型可换，业务照常', '业务只认 work 这样的逻辑模型。底层模型、账号和线路在 Ferry 中调整，客户端配置不用跟着改。Hermes 只是一个接入案例。', '/guide/logical-models', '逻辑模型配置说明'],
  ['autoreset', '重置券快过期，自动兑换', 'AutoReset 在到期阈值内兑换可用的 Codex 重置券，并记录结果。默认提前 20 分钟，可关闭。', '/guide/autoreset', 'AutoReset 设置说明'],
])
const capabilities = computed(() => en.value ? [
  ['01', 'Wait through rate limits', 'Keep eligible requests pending within a bounded recovery window.'],
  ['02', 'Recover from upstream failures', 'Use a compatible fallback when the route permits recovery.'],
  ['03', 'Control concurrency', 'Bound active requests and queue traffic at capacity.'],
  ['04', 'Let agents help operate it', 'Use the Skill and management API to inspect, simulate and apply authorized changes.'],
  ['05', 'Route by tested capabilities', 'Match the endpoint, model and protocol to the task.'],
  ['06', 'Keep a stable model name', 'Change routes without changing the model name in every client.'],
  ['07', 'Connect three protocols', 'Chat, Responses and Messages, within each route’s capabilities.'],
  ['08', 'Inspect every attempt', 'Keep routing decisions, recovery, usage and cost evidence together.'],
] : [
  ['01', '限流等待，自动续接', '符合策略的请求在恢复窗口内等待，条件允许后继续。'],
  ['02', '上游故障，自动接力', '遇到可恢复故障，交给合适的备用线路。'],
  ['03', '并发可控，高峰排队', '限制同时处理的请求，为突发流量留出等待空间。'],
  ['04', 'AI 友好，自助运维', '用 Skill 和管理 API 查证据、模拟选路、执行授权变更。'],
  ['05', '能力实测，按需匹配', '根据实际端点、模型和协议的能力证据选路。'],
  ['06', '入口不变，模型可换', '客户端保留逻辑模型名，上游和线路在 Ferry 中调整。'],
  ['07', '三种协议，统一接入', 'Chat、Responses、Messages，按实际能力转换。'],
  ['08', '请求过程，有据可查', '查看每次尝试、恢复原因、用量和费用依据。'],
])
</script>

<template>
  <main class="ferry-home">
    <section class="ferry-hero ferry-wrap">
      <div class="ferry-eyebrow">OPEN SOURCE · SELF-HOSTED · AI GATEWAY</div>
      <h1 v-if="en">Less time retrying.<br><span>More time for your AI.</span></h1>
      <h1 v-else>少守着重试，<br><span>让 AI 继续干活。</span></h1>
      <p class="ferry-intro">{{ en ? 'Connect your clients to the accounts you already have. Ferry handles routing, waiting and recoverable failures.' : '接上你常用的客户端和已有账号。选路、等待、可恢复的故障，交给 Ferry 处理。' }}</p>
      <div class="ferry-hero-actions"><a class="ferry-button primary" :href="link('/guide/docker')">{{ en ? 'Install with Docker' : '用 Docker 开始' }} ↗</a><a class="ferry-button secondary" href="https://github.com/hulkbig/ferry">{{ en ? 'View on GitHub' : '查看源码' }} →</a></div>
      <div class="ferry-facts">Go + SQLite <span>·</span> AMD64 / ARM64 <span>·</span> Apache-2.0</div>
    </section>

    <section class="ferry-wrap flow-section" id="flows"><div class="ferry-section-heading"><div><div class="ferry-eyebrow">REQUEST → RESPONSE → RECOVERY</div><h2>{{ en ? 'Six ways to use Ferry' : 'Ferry 可以怎么用？' }}</h2></div><p>{{ en ? 'Requests, responses, fallback, queues, logical models and reset credits.' : '请求、返回、接力、排队、逻辑模型、重置券。' }}</p></div><div class="flow-grid"><FlowCard v-for="item in flows" :key="language + item[0]" :scene="item[0]" :language="language" :heading="item[1]" :description="item[2]" :guide="item[3] ? link(item[3]) : undefined" :guide-label="item[4]"/></div><p class="ferry-note">{{ en ? 'Illustrations, not live traffic. Recovery has time and attempt budgets, honors cancellation and cannot splice another upstream stream after visible output is committed.' : '动画不是实时流量。恢复受时间、次数和客户端取消约束；已经提交可见输出的流，不会拼接另一条上游流。' }}</p></section>

    <section class="capability-section"><div class="ferry-wrap"><div class="ferry-eyebrow">FERRY / CAPABILITIES</div><h2>{{ en ? 'What Ferry handles' : 'Ferry 支持什么' }}</h2><div class="capability-grid"><article v-for="item in capabilities" :key="item[0]"><span>{{ item[0] }}</span><h3>{{ item[1] }}</h3><p>{{ item[2] }}</p></article></div></div></section>

    <section class="ferry-wrap start-section"><div><div class="ferry-eyebrow">GET STARTED</div><h2>{{ en ? 'Two commands to start.' : '两条命令，启动 Ferry。' }}</h2><p>{{ en ? 'Create the administrator, add an upstream and issue a client key. No source build or separate database is required.' : '创建管理员，添加上游，再给客户端一个 Key。不用编译源码，也不用另装数据库。' }}</p><a class="ferry-text-link" :href="link('/guide/docker')">{{ en ? 'Installation, backups and upgrades' : '安装、备份与升级指南' }} →</a></div><div class="start-terminal"><div><span>TERMINAL</span><button @click="copy">{{ copyState || (en ? 'Copy' : '复制') }}</button></div><pre><code>{{ command }}</code></pre><p>{{ en ? 'Open localhost:17910 to initialize.' : '打开 localhost:17910 完成初始化。' }}</p><p>{{ en ? 'Release preparation: the image and download will be available after public verification.' : '发布准备中：镜像与下载将在公开验收后提供。' }}</p></div></section>

    <section class="ferry-wrap home-help"><h2>{{ en ? 'Before you start' : '开始之前' }}</h2><details><summary>{{ en ? 'Does Ferry include accounts or credits?' : 'Ferry 自带账号或额度吗？' }}</summary><p>{{ en ? 'No. Use your own API keys or supported subscriptions. Calls, retries and capability probes can incur upstream charges.' : '不提供。你使用自己的 API Key 或受支持的订阅账号；调用、重试和探测可能产生费用。' }}</p></details><details><summary>{{ en ? 'Will Thinking continue until success?' : '会一直 Thinking 到成功吗？' }}</summary><p>{{ en ? 'Waiting is bounded by recovery budgets. A client’s Thinking display is not evidence of continuous upstream reasoning.' : '等待有恢复预算。客户端显示 Thinking，不代表上游一直在推理，也不保证最终成功。' }}</p></details><details><summary>{{ en ? 'Are all combinations verified?' : '所有组合都验证过了吗？' }}</summary><p>{{ en ? 'No. Some account drivers have offline coverage only. Verify tools, streaming and follow-up turns on your own routes.' : '没有。部分账号驱动只有离线测试；工具、流式和续轮要按你实际使用的线路验证。' }}</p></details><div class="help-links"><a :href="link('/guide/faq')">{{ en ? 'Full FAQ' : '完整常见问题' }} →</a><a :href="link('/contributing')">{{ en ? 'Contributing & PRs' : '贡献与 PR' }} →</a><a :href="link('/security')">{{ en ? 'Security reports' : '安全报告' }} →</a></div></section>
  </main>
</template>
