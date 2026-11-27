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
const draftFormat = ref('standard')
const graderMode = ref('')
const language = ref('posix_sh')
const packageFiles = ref<string[]>([])
const packageName = ref('')
const packageText = ref('')
const packageLoaded = ref(false)

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
  packageFiles.value = []
  packageName.value = ''
  packageText.value = ''
  packageLoaded.value = false
}

onMounted(() => run(async () => {
  problem.value = await api<Problem>(path)
  await list()
  const first = revisions.value[0]
  if (first) await select(first)
}))

const revisionPath = (item: Revision) => `${path}/revisions/${item.id}`
const packagePath = (item: Revision) => `${revisionPath(item)}/package/files`

async function refreshPackage(item: Revision) {
  packageFiles.value = (await api<{ files: string[] }>(packagePath(item))).files
  packageLoaded.value = true
}

function openPackageEditor(event: Event) {
  const item = revision.value
  if (item && (event.target as HTMLDetailsElement).open && !packageLoaded.value) {
    void run(() => refreshPackage(item))
  }
}

const openPackageFile = (name: string) => run(async () => {
  if (!revision.value) return
  const result = await api<{ files: string[]; text: string }>(`${packagePath(revision.value)}?${new URLSearchParams({ name })}`)
  packageName.value = name
  packageText.value = result.text
})

const editPackageFile = (remove: boolean) => run(async () => {
  if (!revision.value || !packageName.value) return
  const name = packageName.value
  await api(packagePath(revision.value), {
    method: 'PATCH', body: JSON.stringify(remove ? { name, remove: true } : { name, text: packageText.value }),
  })
  if (remove) { packageName.value = ''; packageText.value = '' }
  await refreshPackage(revision.value)
  message.value = remove ? 'File removed from the draft.' : 'Draft file saved.'
})
const create = () => run(async () => {
  const draft = await api<Revision>(`${path}/revisions`, { method: 'POST', body: JSON.stringify({ execution_format: draftFormat.value, grader_mode: graderMode.value || undefined }) })
  revisions.value.unshift(draft)
  await select(draft)
})

const saveStatement = () => run(async () => {
  const item = revision.value
  if (!item) return
  const target = revisionPath(item)
  const rendered = await api<Statement>(`${target}/statements/en`, { method: 'PUT', body: JSON.stringify({ source: source.value }) })
  statements.value = [...statements.value.filter(s => s.locale !== 'en'), rendered]
  message.value = 'Statement saved.'
})

const changeLanguage = (method: 'PUT' | 'DELETE') => run(async () => {
  if (!revision.value) return
  await api(`${revisionPath(revision.value)}/languages/${language.value}`, { method })
  message.value = `${language.value} ${method === 'PUT' ? 'enabled' : 'removed'}.`
})

const upload = () => run(async () => {
  if (!revision.value || !file.value) return
  if (file.value.size > 16 * 1024 * 1024) throw new Error('ZIP must not exceed 16 MiB.')
  const body = new FormData()
  body.set('file', file.value)
  await api(`${revisionPath(revision.value)}/package`, { method: 'PUT', body })
  if (packageLoaded.value) await refreshPackage(revision.value)
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
    </div>
    <form v-if="problem.can_manage" class="editor-form compact-form" @submit.prevent="create">
      <label class="term-field"><span>Draft format</span><select v-model="draftFormat"><option value="standard">Standard</option><option value="scripted_lua">Scripted Lua</option></select></label>
      <label class="term-field"><span>Grader trigger</span><select v-model="graderMode"><option value="">Disabled</option><option value="manual">Manual</option><option value="automatic_submission">Automatic preview</option></select></label>
      <TermButton type="submit" :disabled="busy">New revision draft</TermButton>
    </form>
    <template v-if="revision">
      <p>{{ revision.execution_format }} · {{ revision.status }}</p>
      <p v-if="revision.older_pins" class="muted">{{ revision.older_pins }} contest links still pin older revisions.</p>
      <template v-if="problem.can_manage && revision.status === 'draft'">
        <form class="editor-form compact-form" @submit.prevent="saveStatement">
          <label class="term-field"><span>English statement (Markdown)</span><textarea v-model="source" rows="8" required :disabled="busy" /></label>
          <TermButton type="submit" :disabled="busy || !source.trim()">Save statement</TermButton>
        </form>
        <div class="editor-form compact-form">
          <label class="term-field"><span>Language</span><select v-model="language"><option v-for="code in ['posix_sh', 'javascript', 'cpp23', 'python314', 'python3', 'cpp17', 'java17']" :key="code">{{ code }}</option></select></label>
          <TermButton :disabled="busy" @click="changeLanguage('PUT')">Enable language</TermButton>
          <TermButton :disabled="busy" @click="changeLanguage('DELETE')">Remove language</TermButton>
        </div>
        <form :key="revision.id" class="editor-form compact-form" @submit.prevent="upload">
          <p>Upload a ZIP up to 16 MiB with <code>{{ revision.execution_format === 'standard' ? 'standard.json' : 'runner.lua' }}</code> at the root and all referenced test files.</p>
          <input type="file" aria-label="Execution package ZIP" accept=".zip,application/zip" :disabled="busy" @change="file = ($event.target as HTMLInputElement).files?.[0]">
          <TermButton type="submit" :disabled="busy || !file">Upload ZIP</TermButton>
        </form>
        <details :key="`package-${revision.id}`" class="editor-form compact-form" @toggle="openPackageEditor">
          <summary>Edit package files</summary>
          <ul v-if="packageFiles.length">
            <li v-for="name in packageFiles" :key="name"><TermButton :disabled="busy" @click="openPackageFile(name)">{{ name }}</TermButton></li>
          </ul>
          <form @submit.prevent="editPackageFile(false)">
            <label class="term-field"><span>Relative file path</span><input v-model="packageName" :disabled="busy" required placeholder="tests/input"></label>
            <label class="term-field"><span>File contents (UTF-8, up to 256 KiB)</span><textarea v-model="packageText" rows="6" spellcheck="false" :disabled="busy" /></label>
            <div class="button-line">
              <TermButton type="submit" :disabled="busy || !packageName">Save file</TermButton>
              <TermButton type="button" :disabled="busy || !packageFiles.includes(packageName)" @click="editPackageFile(true)">Remove file</TermButton>
            </div>
          </form>
        </details>
        <TermButton variant="primary" :disabled="busy" @click="publish">Validate and publish</TermButton>
      </template>
      <article v-for="statement in statements" :key="statement.locale" class="material-entry">
        <h3>Statement · {{ statement.locale }}</h3><div class="rendered-document" v-html="statement.html" />
      </article>
    </template>
  </section>
</template>
