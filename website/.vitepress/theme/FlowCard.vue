<script setup>
import { ref, onMounted, onBeforeUnmount } from 'vue'
import requestZH from '../../public/assets/flow-request-zh.svg?raw'
import requestEN from '../../public/assets/flow-request-en.svg?raw'
import responseZH from '../../public/assets/flow-response-zh.svg?raw'
import responseEN from '../../public/assets/flow-response-en.svg?raw'
import fallbackZH from '../../public/assets/flow-fallback-zh.svg?raw'
import fallbackEN from '../../public/assets/flow-fallback-en.svg?raw'
import queueZH from '../../public/assets/flow-queue-zh.svg?raw'
import queueEN from '../../public/assets/flow-queue-en.svg?raw'
import logicalZH from '../../public/assets/flow-logical-zh.svg?raw'
import logicalEN from '../../public/assets/flow-logical-en.svg?raw'
import autoresetZH from '../../public/assets/flow-autoreset-zh.svg?raw'
import autoresetEN from '../../public/assets/flow-autoreset-en.svg?raw'
const props = defineProps({ scene: String, language: String, heading: String, description: String, guide: String, guideLabel: String })
const graphic = ref(null), playing = ref(true)
const sources = { zh: { request: requestZH, response: responseZH, fallback: fallbackZH, queue: queueZH, logical: logicalZH, autoreset: autoresetZH }, en: { request: requestEN, response: responseEN, fallback: fallbackEN, queue: queueEN, logical: logicalEN, autoreset: autoresetEN } }
let svg, observer, preference, inView = false
function sync() {
  if (!svg) return
  svg.classList.toggle('motion-enabled', playing.value)
  if (playing.value && inView && !document.hidden) svg.unpauseAnimations()
  else svg.pauseAnimations()
}
function toggle() { playing.value = !playing.value; sync() }
function replay() { if (!svg) return; svg.setCurrentTime(0); playing.value = true; sync() }
function motionChange() { if (preference.matches) playing.value = false; sync() }
onMounted(() => {
  svg = graphic.value.querySelector('svg')
  preference = matchMedia('(prefers-reduced-motion: reduce)')
  playing.value = !preference.matches
  svg.pauseAnimations()
  observer = new IntersectionObserver(([entry]) => { inView = entry.isIntersecting; sync() }, { threshold: .05 })
  observer.observe(graphic.value)
  preference.addEventListener('change', motionChange)
  document.addEventListener('visibilitychange', sync)
  sync()
})
onBeforeUnmount(() => { observer?.disconnect(); preference?.removeEventListener('change', motionChange); document.removeEventListener('visibilitychange', sync) })
</script>

<template>
  <article class="flow-card" :data-flow="scene">
    <div class="flow-heading"><h3>{{ heading }}</h3><div class="flow-actions"><button type="button" @click="toggle" :aria-label="(playing ? (language === 'zh' ? '暂停' : 'Pause') : (language === 'zh' ? '播放' : 'Play')) + ' ' + heading">{{ playing ? (language === 'zh' ? '暂停' : 'Pause') : (language === 'zh' ? '播放' : 'Play') }}</button><button type="button" @click="replay">{{ language === 'zh' ? '重播' : 'Replay' }}</button></div></div>
    <div ref="graphic" class="flow-svg" v-html="sources[language][scene]"></div>
    <p>{{ description }}</p>
    <a v-if="guide" class="ferry-text-link flow-guide" :href="guide">{{ guideLabel }} →</a>
  </article>
</template>
