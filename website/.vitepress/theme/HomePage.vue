<script setup>
import { computed, ref } from 'vue'
import { withBase } from 'vitepress'
import FlowCard from './FlowCard.vue'
const props = defineProps({ language: { type: String, default: 'zh' } })
const en = computed(() => props.language === 'en')
const link = path => withBase((en.value ? '/en' : '') + path + (path.endsWith('/') ? '' : '.html'))
const copyState = ref('')
const command = 'curl -fsSLo compose.yaml https://github.com/hulkbig/ferry/releases/latest/download/compose.yaml\ndocker compose up -d --no-build'
async function copy() { try { await navigator.clipboard.writeText(command); copyState.value = en.value ? 'Copied' : '已复制' } catch { copyState.value = en.value ? 'Select and copy the commands' : '请选中命令复制' } }
const reliability = computed(() => en.value ? [
  ['fallback', 'Automatic failover', 'When the primary upstream fails, automatically try a compatible fallback.'],
  ['queue', 'Queue requests, see fewer 429s', 'Wait within configured budgets when capacity is unavailable, then continue when it opens.'],
  ['autoreset', 'Automatic resets and quota protection', 'Automatically redeem available Codex reset credits and reduce requests to exhausted accounts.', '/guide/autoreset', 'Configure AutoReset'],
] : [
  ['fallback', '故障自动切换', '首选上游故障时，自动尝试兼容的备用线路。'],
  ['queue', '限流排队，少看 429', '容量不足时按预算排队等待，容量释放后继续请求。'],
  ['autoreset', '自动重置，额度保护', '自动兑换可用的 Codex 重置券，结合配额保护，减少额度耗尽后的无效请求。', '/guide/autoreset', '自动重置与额度保护'],
])
</script>

<template>
  <main class="ferry-home">
    <section class="ferry-hero ferry-wrap">
      <div class="ferry-eyebrow">OPEN SOURCE · SELF-HOSTED · AI GATEWAY</div>
      <h1 v-if="en">Ferry<br><span>Self-hosted AI gateway</span></h1>
      <h1 v-else>Ferry<br><span>自托管 AI 网关</span></h1>
      <p class="ferry-intro">{{ en ? 'Connect your clients to the accounts you already have. Ferry handles routing, waiting and recoverable failures.' : '接上你常用的客户端和已有账号。选路、等待、可恢复的故障，交给 Ferry 处理。' }}</p>
      <div class="ferry-hero-actions"><a class="ferry-button primary" :href="link('/guide/docker')">{{ en ? 'Install with Docker' : '用 Docker 开始' }} ↗</a><a class="ferry-button secondary" href="https://github.com/hulkbig/ferry">{{ en ? 'View on GitHub' : '查看源码' }} →</a></div>
      <div class="ferry-facts">Go + SQLite <span>·</span> AMD64 / ARM64 <span>·</span> Apache-2.0</div>
    </section>

    <section class="ferry-wrap chapter-section" id="ai-first">
      <div class="ferry-section-heading"><div><div class="ferry-eyebrow">01 / AI FIRST</div><h2>AI First</h2></div></div>
      <div class="flow-grid">
        <article class="flow-card">
          <div class="flow-heading"><h3>{{ en ? 'Official Skill, full functionality' : '官方 Skill，全功能支持' }}</h3></div>
          <img class="chapter-figure" :src="withBase('/assets/perf-skill-' + language + '.svg')" :alt="en ? 'Official Skill capabilities' : '官方 Skill 全功能支持'" width="960" height="480" loading="lazy" />
          <p>{{ en ? "From models, accounts and routes to request diagnostics, quota management and daily operations, agents access Ferry’s full management capabilities through the official Skill." : '从模型、账号和路由配置，到请求诊断、额度管理和日常运维，Agent 通过官方 Skill 使用 Ferry 的完整管理能力。' }}</p>
          <a class="ferry-text-link" href="https://github.com/hulkbig/ferry/blob/main/docs/guide/agent-skill.md">{{ en ? 'Install the official Skill' : '安装官方 Skill' }} →</a>
        </article>
        <FlowCard scene="logical" :language="language" :heading="en ? 'Logical models: built around your business' : '逻辑模型：对业务，不对上游'" :description="en ? 'Define model endpoints for your business. Change upstream models, accounts and routes inside Ferry while keeping your application configuration unchanged.' : '按业务定义模型入口。上游模型、账号和线路在 Ferry 内调整，业务端配置保持不变。'" :guide="link('/guide/logical-models')" :guide-label="en ? 'Configure logical models' : '配置逻辑模型'" />
      </div>
    </section>

    <section class="ferry-wrap chapter-section" id="performance">
      <div class="ferry-section-heading"><div><div class="ferry-eyebrow">02 / PERFORMANCE</div><h2>{{ en ? 'Extreme Performance' : '极致性能' }}</h2></div></div>
      <div class="flow-grid">
        <article class="flow-card">
          <div class="flow-heading"><h3>{{ en ? 'Lower memory usage' : '内存占用更低' }}</h3></div>
          <img class="chapter-figure" :src="withBase('/assets/perf-peak_working_set_mib-' + language + '.svg')" :alt="en ? 'Peak memory comparison' : '峰值内存占用对比'" width="960" height="480" loading="lazy" />
          <p>{{ en ? 'Peak working set: 30.8–34.5 MiB. About 49%–55% lower than CLIProxyAPI and 92%–93% lower than the sub2api stack in this benchmark.' : '峰值内存工作集 30.8–34.5 MiB。在本次基准测试中，比 CLIProxyAPI 低约 49%–55%，比 sub2api 整套服务低约 92%–93%。' }}</p>
        </article>
        <article class="flow-card">
          <div class="flow-heading"><h3>{{ en ? 'Lower CPU usage' : 'CPU 占用更低' }}</h3></div>
          <img class="chapter-figure" :src="withBase('/assets/perf-peak_cpu_percent-' + language + '.svg')" :alt="en ? 'Peak CPU comparison' : '峰值 CPU 占用率对比'" width="960" height="480" loading="lazy" />
          <p>{{ en ? 'Ferry’s peak CPU usage is close to CLIProxyAPI and lower than the sub2api stack: 146.6%–157.1%, 151.0%–151.2% and 390.2%–406.3%, respectively.' : 'Ferry 峰值 CPU 占用与 CLIProxyAPI 接近，低于 sub2api 整套服务：分别为 146.6%–157.1%、151.0%–151.2%、390.2%–406.3%。' }}</p>
        </article>
      </div>
      <p class="ferry-note">{{ en ? 'OpenAI Chat Mock benchmark · ARM64 · concurrency 32 · 1 KiB / 64 KiB responses · 2026-10-30.' : '基准测试：OpenAI Chat Mock · ARM64 · 并发 32 · 1 KiB / 64 KiB 响应 · 2026-10-30。' }} <a :href="link('/guide/performance')">{{ en ? 'Methods & full data →' : '方法与完整数据 →' }}</a></p>
    </section>

    <section class="ferry-wrap chapter-section" id="reliability">
      <div class="ferry-section-heading"><div><div class="ferry-eyebrow">03 / RELIABILITY</div><h2>{{ en ? 'Reliable Requests' : '可靠请求' }}</h2></div></div>
      <div class="flow-grid">
        <FlowCard :scene="reliability[0][0]" :language="language" :heading="reliability[0][1]" :description="reliability[0][2]" />
        <article class="flow-card">
          <div class="flow-heading"><h3>{{ en ? 'Configurable switching policies' : '切换策略自由配置' }}</h3></div>
          <img class="chapter-figure" :src="withBase('/assets/perf-strategy-' + language + '.svg')" :alt="en ? 'Switching policies' : '切换策略'" width="960" height="480" loading="lazy" />
          <p>{{ en ? 'Choose when to wait or switch using time windows, first-output timeouts and failure types.' : '按时间窗口、首输出等待上限和失败类型等条件，配置何时等待、何时切换。' }}</p>
          <a class="ferry-text-link" href="https://github.com/hulkbig/ferry/blob/main/docs/guide/long-tasks.md">{{ en ? 'Configure switching policies' : '配置切换策略' }} →</a>
        </article>
        <FlowCard v-for="item in reliability.slice(1)" :key="language + item[0]" :scene="item[0]" :language="language" :heading="item[1]" :description="item[2]" :guide="item[3] ? link(item[3]) : undefined" :guide-label="item[4]" />
      </div>
    </section>

    <section class="ferry-wrap start-section"><div><div class="ferry-eyebrow">GET STARTED</div><h2>{{ en ? 'Two commands to start.' : '两条命令，启动 Ferry。' }}</h2><p>{{ en ? 'Create the administrator, add an upstream and issue a client key. No source build or separate database is required.' : '创建管理员，添加上游，再给客户端一个 Key。不用编译源码，也不用另装数据库。' }}</p><a class="ferry-text-link" :href="link('/guide/docker')">{{ en ? 'Installation, backups and upgrades' : '安装、备份与升级指南' }} →</a></div><div class="start-terminal"><div><span>TERMINAL</span><button @click="copy">{{ copyState || (en ? 'Copy' : '复制') }}</button></div><pre><code>{{ command }}</code></pre><p>{{ en ? 'Open localhost:17910 to initialize.' : '打开 localhost:17910 完成初始化。' }}</p></div></section>

    <section class="ferry-wrap home-help"><h2>{{ en ? 'Before you start' : '开始之前' }}</h2><details><summary>{{ en ? 'Does Ferry include accounts or credits?' : 'Ferry 自带账号或额度吗？' }}</summary><p>{{ en ? 'No. Use your own API keys or supported subscriptions. Calls, retries and capability probes can incur upstream charges.' : '不提供。你使用自己的 API Key 或受支持的订阅账号；调用、重试和探测可能产生费用。' }}</p></details><details><summary>{{ en ? 'Will Thinking continue until success?' : '会一直 Thinking 到成功吗？' }}</summary><p>{{ en ? 'Waiting is bounded by recovery budgets. A client’s Thinking display is not evidence of continuous upstream reasoning.' : '等待有恢复预算。客户端显示 Thinking，不代表上游一直在推理，也不保证最终成功。' }}</p></details><details><summary>{{ en ? 'Are all combinations verified?' : '所有组合都验证过了吗？' }}</summary><p>{{ en ? 'No. Some account drivers have offline coverage only. Verify tools, streaming and follow-up turns on your own routes.' : '没有。部分账号驱动只有离线测试；工具、流式和续轮要按你实际使用的线路验证。' }}</p></details><div class="help-links"><a :href="link('/guide/faq')">{{ en ? 'Full FAQ' : '完整常见问题' }} →</a><a :href="link('/contributing')">{{ en ? 'Contributing & PRs' : '贡献与 PR' }} →</a><a :href="link('/security')">{{ en ? 'Security reports' : '安全报告' }} →</a></div></section>
  </main>
</template>

<style scoped>
.chapter-section { padding-top: 40px; padding-bottom: 48px; }
.chapter-figure { display: block; width: 100%; height: auto; border-radius: 13px; }
.chapter-section .ferry-note a { color: var(--vp-c-brand-1); }
</style>
