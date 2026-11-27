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
  execution_available: boolean
  can_manage_submissions: boolean
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
const mode = ref<'editor' | 'file'>('editor')
const code = ref('')
const sourceBytes = computed(() => mode.value === 'file' ? file.value?.size ?? 0 : new TextEncoder().encode(code.value).length)
const source = ref<{ id: string; text?: string; error?: string }>()
const next = ref('')
const listing = ref(false)
const language = ref('')
const error = ref('')
const busy = ref(false)
const cancelling = ref('')
const rejudging = ref('')
const showAll = ref(false)
let streams: EventSource[] = []
const statement = computed(() => task.value?.statements[0]?.html ?? '')

async function load() {
  try {
    task.value = await api<Task>(`/contests/${contestID}/problems/${problemID}`)
    language.value ||= task.value.languages[0]?.code ?? ''
    const path = `/contests/${contestID}/problems/${problemID}/submissions`
    source.value = undefined
    const page = await api<Page<Submission>>(showAll.value ? path : `${path}/me`)
    submissions.value = page.items
    next.value = page.next ?? ''
    streams.forEach(stream => stream.close())
    streams = []
    for (const submission of submissions.value) subscribe(submission)
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Problem is unavailable'
  }
}
function subscribe(submission: Submission) {
      void loadAttempts(submission.id).catch(() => {})
      const stream = new EventSource(`/api/v1/submissions/${submission.id}/events`)
      stream.addEventListener('submission', event => {
        try {
          const items = JSON.parse((event as MessageEvent).data) as Attempt[]
          attempts.value[submission.id] = items
          if (items.length && items.every(item => ['finished', 'error', 'timeout', 'cancelled'].includes(item.status))) stream.close()
        } catch { /* Ignore malformed updates. */ }
      })
      streams.push(stream)
}
async function more() {
  if (!showAll.value || !next.value || listing.value) return
  listing.value = true
  try {
    const page = await api<Page<Submission>>(`/contests/${contestID}/problems/${problemID}/submissions?after=${encodeURIComponent(next.value)}`)
    if (!showAll.value) return
    submissions.value.push(...page.items)
    next.value = page.next ?? ''
    page.items.forEach(subscribe)
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Could not load submissions'
  } finally { listing.value = false }
}
async function viewSource(id: string) {
  if (source.value?.id === id) { source.value = undefined; return }
  const request = { id }
  source.value = request
  try {
    const response = await fetch(`/api/v1/submissions/${id}/source`, { cache: 'no-store' })
    if (!response.ok) throw new Error(`Source is unavailable (${response.status})`)
    const text = await response.text()
    if (source.value?.id === id) source.value = { id, text }
  } catch (cause) {
    if (source.value?.id === id) source.value = { id, error: cause instanceof Error ? cause.message : 'Could not open source' }
  }
}
async function loadAttempts(submission: string) {
  attempts.value[submission] = (await api<Page<Attempt>>(`/submissions/${submission}/attempts`)).items
}
async function cancelAttempt(submission: string, attempt: string) {
  cancelling.value = attempt
  error.value = ''
  try {
    await api(`/submissions/${submission}/attempts/${attempt}/cancel`, { method: 'POST' })
    await loadAttempts(submission)
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Cancellation failed'
  } finally {
    cancelling.value = ''
  }
}
async function rejudge(submission: string) {
  rejudging.value = submission
  error.value = ''
  try {
    await api(`/submissions/${submission}/rejudge`, { method: 'POST', body: JSON.stringify({ problem_revision_id: task.value?.problem_revision_id }) })
    await load()
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Rejudge failed'
  } finally { rejudging.value = '' }
}
async function submit() {
  if (busy.value || !task.value?.execution_available || !sourceBytes.value || sourceBytes.value > 256 * 1024 || !language.value) return
  busy.value = true
  error.value = ''
  try {
    const body = new FormData()
    if (mode.value === 'file' && file.value) body.set('source', file.value)
    else body.set('source', new Blob([code.value], { type: 'text/plain;charset=utf-8' }), 'source.txt')
    body.set('language', language.value)
    await upload(`/contests/${contestID}/problems/${problemID}/submissions`, body)
    file.value = null
    code.value = ''
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
    <form v-if="task.execution_available" class="submit-panel" @submit.prevent="submit">
      <p class="eyebrow">SUBMIT SOLUTION</p>
      <label class="term-field"><span>Language</span><select v-model="language"><option v-for="item in task.languages" :key="item.code" :value="item.code">{{ item.title }}</option></select></label>
      <label class="term-field"><span>Source method</span><select v-model="mode" :disabled="busy"><option value="editor">Code editor</option><option value="file">Upload file</option></select></label>
      <label v-if="mode === 'editor'" class="term-field"><span>Source code</span><textarea v-model="code" class="code-editor" rows="10" spellcheck="false" autocapitalize="off" :disabled="busy" /></label>
      <label v-else class="file-picker"><input type="file" :disabled="busy" @change="file = ($event.target as HTMLInputElement).files?.[0] ?? null"><span>{{ file?.name ?? 'Choose source file' }}</span></label>
      <p v-if="sourceBytes > 256 * 1024" class="notice">Source exceeds 256 KiB.</p>
      <TermButton type="submit" variant="primary" :disabled="busy || !language || !sourceBytes || sourceBytes > 256 * 1024">{{ busy ? 'Sending…' : 'Submit' }}</TermButton>
    </form>
    <p v-else class="notice">This revision does not accept submissions.</p>
  </section>
  <section class="section-band">
    <div class="section-heading"><div><p class="eyebrow">YOUR WORK</p><h2>Submissions</h2></div></div>
    <label v-if="task?.can_manage_submissions"><input v-model="showAll" type="checkbox" @change="load"> Show all submissions</label>
    <div v-for="submission in submissions" :key="submission.id" class="submission-row">
      <div><strong>{{ submission.language }}</strong><span>{{ new Date(submission.submitted_at).toLocaleString() }}</span><span v-if="showAll">{{ submission.id }}</span></div>
      <TermButton @click="viewSource(submission.id)">{{ source?.id === submission.id ? 'Hide source' : 'View source' }}</TermButton>
      <section v-if="source?.id === submission.id" aria-label="Submission source">
        <p v-if="source.error" role="alert">{{ source.error }}</p>
        <pre v-else-if="source.text !== undefined" class="source-view"><code>{{ source.text }}</code></pre>
        <p v-else role="status">Loading source…</p>
        <a :href="`/api/v1/submissions/${submission.id}/source`" download="source.txt">Download original source</a>
      </section>
      <TermButton v-if="task?.can_manage_submissions && task.execution_available" :disabled="!!rejudging" @click="rejudge(submission.id)">Rejudge on pinned revision</TermButton>
      <div v-for="attempt in attempts[submission.id] ?? []" :key="attempt.id">
        <div class="attempt-chip">
          <span>{{ attempt.cancel_pending ? 'Cancellation requested' : attempt.active_tests?.length ? `Testing ${attempt.active_tests.join(', ')}` : (attempt.phase ?? attempt.status) }}</span>
          <span v-if="attempt.verdict">{{ attempt.verdict === 'testing_error' ? 'Ошибка тестирования' : attempt.verdict }}</span>
          <span v-if="attempt.base_score">{{ attempt.base_score }}</span>
          <TermButton v-if="attempt.can_cancel" :disabled="!!cancelling" @click="cancelAttempt(submission.id, attempt.id)">Cancel</TermButton>
        </div>
        <details v-if="attempt.protocol || attempt.tests?.length">
          <summary>{{ attempt.protocol ? 'Judging protocol' : 'Test verdicts' }}</summary>
          <dl v-if="attempt.protocol">
            <dt>Attempt</dt><dd>#{{ attempt.attempt_number }}</dd>
            <dt>Judged revision</dt><dd>{{ attempt.protocol.problem_revision_id }}</dd>
            <dt>Started</dt><dd>{{ attempt.protocol.started_at ? new Date(attempt.protocol.started_at).toLocaleString() : '—' }}</dd>
            <dt>Finished</dt><dd>{{ attempt.protocol.finished_at ? new Date(attempt.protocol.finished_at).toLocaleString() : '—' }}</dd>
          </dl>
          <ul v-if="attempt.tests?.length">
            <li v-for="test in attempt.tests" :key="test.test_id">{{ test.test_id }} · {{ test.verdict }} <span v-if="test.score != null">· {{ test.score }}</span><pre v-if="test.checker_metadata && Object.keys(test.checker_metadata).length" class="source-view">{{ JSON.stringify(test.checker_metadata, null, 2) }}</pre></li>
          </ul>
          <template v-if="attempt.protocol?.groups?.length"><h4>Subgroup scores</h4><ul><li v-for="group in attempt.protocol.groups" :key="group.subgroup_id">{{ group.subgroup_id }} · {{ group.score }}</li></ul></template>
          <ul v-if="attempt.artifacts?.length">
            <li v-for="artifact in attempt.artifacts" :key="artifact.id"><a :href="`/api/v1/submissions/${submission.id}/artifacts/${artifact.id}`">Download {{ artifact.kind }}</a></li>
          </ul>
        </details>
      </div>
    </div>
    <TermButton v-if="showAll && next" :disabled="listing" @click="more">Load more submissions</TermButton>
    <p v-if="!submissions.length" class="muted">No submissions for this problem.</p>
  </section>
</template>

<style scoped>
.code-editor, .source-view { font-family: monospace; tab-size: 4; }
.code-editor { resize: vertical; }
.source-view { max-width: 100%; max-height: 28rem; overflow: auto; white-space: pre; }
</style>
