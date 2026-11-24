<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api, upload } from '../api/client'
import type { Attempt, Page, Submission } from '../api/types'
import TermButton from '../components/termui/TermButton.vue'

type Task = {
  id: string
  title: string
  problem_revision_id: string
  statements: { locale: string; html: string }[]
  languages: { code: string; title: string }[]
}
const route = useRoute()
const contestID = String(route.params.id)
const problemID = String(route.params.problemId)
const task = ref<Task | null>(null)
const submissions = ref<Submission[]>([])
const attempts = ref<Record<string, Attempt[]>>({})
const file = ref<File | null>(null)
const language = ref('')
const error = ref('')
const busy = ref(false)
let streams: EventSource[] = []
const statement = computed(() => task.value?.statements[0]?.html ?? '')

async function load() {
  try {
    task.value = await api<Task>(`/contests/${contestID}/problems/${problemID}`)
    language.value ||= task.value.languages[0]?.code ?? ''
    submissions.value = (await api<Page<Submission>>(`/contests/${contestID}/problems/${problemID}/submissions/me`)).items
    streams.forEach(stream => stream.close())
    streams = submissions.value.map(submission => {
      const stream = new EventSource(`/api/v1/submissions/${submission.id}/events`)
      stream.addEventListener('submission', event => {
        try { attempts.value[submission.id] = JSON.parse((event as MessageEvent).data) as Attempt[] } catch { /* Ignore malformed updates. */ }
      })
      return stream
    })
    for (const submission of submissions.value) {
      attempts.value[submission.id] = (await api<Page<Attempt>>(`/submissions/${submission.id}/attempts`)).items
    }
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Problem is unavailable'
  }
}
async function submit() {
  if (!file.value || !language.value) return
  busy.value = true
  error.value = ''
  try {
    const body = new FormData()
    body.set('source', file.value)
    body.set('language', language.value)
    await upload(`/contests/${contestID}/problems/${problemID}/submissions`, body)
    file.value = null
    await load()
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Submission failed'
  } finally { busy.value = false }
}
onMounted(load)
onUnmounted(() => streams.forEach(stream => stream.close()))
</script>

<template>
  <p v-if="error" class="notice">{{ error }}</p>
  <section class="page-head">
    <p class="eyebrow">PROBLEM / PINNED REVISION</p>
    <h1>{{ task?.title ?? 'Problem' }}</h1>
    <p>Revision {{ task?.problem_revision_id?.slice(0, 8) }}</p>
  </section>
  <section v-if="task" class="statement-layout">
    <article class="statement rendered-document" v-html="statement" />
    <aside class="submit-panel">
      <p class="eyebrow">SUBMIT SOLUTION</p>
      <label class="term-field"><span>Language</span><select v-model="language"><option v-for="item in task.languages" :key="item.code" :value="item.code">{{ item.title }}</option></select></label>
      <label class="file-picker"><input type="file" @change="file = ($event.target as HTMLInputElement).files?.[0] ?? null"><span>{{ file?.name ?? 'Choose source file' }}</span></label>
      <TermButton variant="primary" :disabled="busy || !file" @click="submit">{{ busy ? 'Sending…' : 'Submit' }}</TermButton>
    </aside>
  </section>
  <section class="section-band">
    <div class="section-heading"><div><p class="eyebrow">YOUR WORK</p><h2>Submissions</h2></div></div>
    <div v-for="submission in submissions" :key="submission.id" class="submission-row">
      <div><strong>{{ submission.language }}</strong><span>{{ new Date(submission.submitted_at).toLocaleString() }}</span></div>
      <div v-for="attempt in attempts[submission.id] ?? []" :key="attempt.id" class="attempt-chip"><span>{{ attempt.status }}</span><span v-if="attempt.verdict">{{ attempt.verdict === 'testing_error' ? 'Ошибка тестирования' : attempt.verdict }}</span><span v-if="attempt.base_score">{{ attempt.base_score }}</span></div>
    </div>
    <p v-if="!submissions.length" class="muted">No submissions for this problem.</p>
  </section>
</template>
