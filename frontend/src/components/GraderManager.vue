<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api } from '../api/client'
import type { ContestProblem, Page } from '../api/types'
import TermButton from './termui/TermButton.vue'

type Run = { id: string; contest_problem_id: string; status: string; mode: string }
type Detail = Run & { log: string; published: boolean; scores: { participant_id: string; score: string }[] }
const props = defineProps<{ contest: string; problems: ContestProblem[] }>()
const problem = ref('')
const runs = ref<Run[]>([])
const current = ref<Detail>()
const busy = ref(false)
const error = ref('')
const path = `/contests/${props.contest}`
async function run(action: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try { await action() } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Grader request failed'
  } finally { busy.value = false }
}
async function reload() {
  runs.value = (await api<Page<Run>>(`${path}/grader-runs`)).items ?? []
  if (current.value) current.value = await api<Detail>(`/grader-runs/${current.value.id}`)
}
onMounted(() => run(reload))
const preview = () => run(async () => {
  const item = await api<Run>(`${path}/problems/${problem.value}/grader-runs`, { method: 'POST', body: JSON.stringify({ mode: 'preview' }) })
  current.value = await api<Detail>(`/grader-runs/${item.id}`)
  await reload()
})
const inspect = (id: string) => run(async () => { current.value = await api<Detail>(`/grader-runs/${id}`) })
const publish = () => run(async () => {
  if (!current.value) return
  await api(`/grader-runs/${current.value.id}/publish`, { method: 'POST' })
  await reload()
})
</script>

<template>
  <section class="editor-form compact-form">
    <p v-if="error" class="notice" role="alert">{{ error }}</p>
    <label class="term-field"><span>Problem</span><select v-model="problem"><option value="">Choose problem with Grader</option><option v-for="item in problems.filter(p => p.grader_mode)" :key="item.id" :value="item.id">{{ item.title }}</option></select></label>
    <TermButton :disabled="busy || !problem" @click="preview">Run preview</TermButton>
    <TermButton :disabled="busy" @click="run(reload)">Refresh runs</TermButton>
    <ul v-if="runs.length"><li v-for="item in runs" :key="item.id"><button :disabled="busy" @click="inspect(item.id)">{{ problems.find(p => p.id === item.contest_problem_id)?.title ?? item.contest_problem_id }} · {{ item.status }} · {{ item.id.slice(0, 8) }}</button></li></ul>
    <section v-if="current">
      <p>{{ current.status }}<template v-if="current.published"> · Published</template></p>
      <pre v-if="current.log">{{ current.log }}</pre>
      <table v-if="current.scores?.length"><thead><tr><th>Participant ID</th><th>Score</th></tr></thead><tbody><tr v-for="score in current.scores" :key="score.participant_id"><td>{{ score.participant_id }}</td><td>{{ score.score }}</td></tr></tbody></table>
      <TermButton v-if="current.status === 'completed' && !current.published" :disabled="busy" @click="publish">Publish scores</TermButton>
    </section>
  </section>
</template>
