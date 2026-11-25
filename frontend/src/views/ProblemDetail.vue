<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { api } from '../api/client'
import type { Page, Problem } from '../api/types'
import TermButton from '../components/termui/TermButton.vue'

type Revision = { id: string; revision_number: number; status: string; execution_format: string; older_pins: number }
type Statement = { locale: string; source?: string; html: string }
const path = `/problems/${String(useRoute().params.id)}`
const problem = ref<Problem>()
const revisions = ref<Revision[]>([])
const revision = ref<Revision>()
const statements = ref<Statement[]>([])
const source = ref('')
const file = ref<File>()
const error = ref('')
const message = ref('')
const busy = ref(false)

async function run(action: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  message.value = ''
  try { await action() } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Request failed'
  } finally { busy.value = false }
}

async function list() {
  revisions.value = (await api<Page<Revision>>(`${path}/revisions`)).items
}

async function select(item: Revision) {
  const data = await api<Page<Statement>>(`${path}/revisions/${item.id}/statements`)
  revision.value = item
  statements.value = data.items
  source.value = data.items.find(s => s.locale === 'en')?.source ?? ''
  file.value = undefined
}

onMounted(() => run(async () => {
  problem.value = await api<Problem>(path)
  await list()
  const first = revisions.value[0]
  if (first) await select(first)
}))

const revisionPath = (item: Revision) => `${path}/revisions/${item.id}`
const create = () => run(async () => {
  const draft = await api<Revision>(`${path}/revisions`, { method: 'POST', body: JSON.stringify({ execution_format: 'scripted_lua' }) })
  revisions.value.unshift(draft)
  await select(draft)
})

const saveStatement = () => run(async () => {
  const item = revision.value
  if (!item) return
  const target = revisionPath(item)
  const rendered = await api<Statement>(`${target}/statements/en`, { method: 'PUT', body: JSON.stringify({ source: source.value }) })
  statements.value = [...statements.value.filter(s => s.locale !== 'en'), rendered]
  await api(`${target}/languages/python3`, { method: 'PUT' })
  message.value = 'Statement saved. Python 3 is enabled.'
})

const upload = () => run(async () => {
  if (!revision.value || !file.value) return
  if (file.value.size > 16 * 1024 * 1024) throw new Error('ZIP must not exceed 16 MiB.')
  const body = new FormData()
  body.set('file', file.value)
  await api(`${revisionPath(revision.value)}/package`, { method: 'PUT', body })
  message.value = 'Package uploaded.'
})

const publish = () => run(async () => {
  const item = revision.value
  if (!item) return
  const target = revisionPath(item)
  await api(`${target}/validate`, { method: 'POST' })
  await api(`${target}/publish`, { method: 'POST' })
  item.status = 'published'
  await list()
  message.value = `Revision ${item.revision_number} published.`
})
</script>

<template>
  <RouterLink to="/problems">← Problems</RouterLink>
  <section v-if="problem" class="page-head">
    <p class="eyebrow">PROBLEM</p><h1>{{ problem.title }}</h1><p>{{ problem.description }}</p>
  </section>
  <p v-if="error" class="notice" role="alert">{{ error }}</p>
  <p v-if="message" class="notice" role="status">{{ message }}</p>
  <p v-if="busy" class="muted" role="status">Working…</p>
  <section v-if="problem" class="section-band">
    <h2>Revisions</h2>
    <p v-if="!revisions.length && !busy" class="muted">No revisions yet.</p>
    <div class="button-line">
      <TermButton v-for="item in revisions" :key="item.id" :variant="revision?.id === item.id ? 'primary' : 'plain'" :disabled="busy" @click="run(() => select(item))">#{{ item.revision_number }} · {{ item.status }}</TermButton>
      <TermButton v-if="problem.can_manage" :disabled="busy" @click="create">New scripted Lua draft</TermButton>
    </div>
    <template v-if="revision">
      <p>{{ revision.execution_format }} · {{ revision.status }}</p>
      <p v-if="revision.older_pins" class="muted">{{ revision.older_pins }} contest links still pin older revisions.</p>
      <template v-if="problem.can_manage && revision.status === 'draft' && revision.execution_format === 'scripted_lua'">
        <form class="editor-form compact-form" @submit.prevent="saveStatement">
          <label class="term-field"><span>English statement (Markdown)</span><textarea v-model="source" rows="8" required :disabled="busy" /></label>
          <TermButton type="submit" :disabled="busy || !source.trim()">Save statement</TermButton>
        </form>
        <form :key="revision.id" class="editor-form compact-form" @submit.prevent="upload">
          <p>Upload a ZIP up to 16 MiB with a non-empty <code>runner.lua</code> at the root.</p>
          <input type="file" aria-label="Scripted Lua ZIP" accept=".zip,application/zip" :disabled="busy" @change="file = ($event.target as HTMLInputElement).files?.[0]">
          <TermButton type="submit" :disabled="busy || !file">Upload ZIP</TermButton>
        </form>
        <TermButton variant="primary" :disabled="busy" @click="publish">Validate and publish</TermButton>
      </template>
      <p v-else-if="revision.status === 'draft' && revision.execution_format === 'standard'" class="muted">Standard draft editing is available through the API.</p>
      <article v-for="statement in statements" :key="statement.locale" class="material-entry">
        <h3>Statement · {{ statement.locale }}</h3><div class="rendered-document" v-html="statement.html" />
      </article>
    </template>
  </section>
</template>
