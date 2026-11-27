<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { api, upload } from '../api/client'
import type { Contest, ContestProblem, Material, Page, Problem, Scoreboard } from '../api/types'
import TermButton from '../components/termui/TermButton.vue'
import ResourceMembers from '../components/ResourceMembers.vue'
import MaterialEditor from '../components/MaterialEditor.vue'
import GraderManager from '../components/GraderManager.vue'

type Revision = { id: string; revision_number: number; status: string; execution_format: string }
type Settings = { status: Contest['status']; starts_at: string | null; ends_at: string | null; self_registration: boolean; scoreboard_visibility: string; max_queued_submissions: number | null }
const id = String(useRoute().params.id)
const path = `/contests/${id}`
const contest = ref<Contest>()
const settings = ref<Settings>()
const problems = ref<ContestProblem[]>([])
const materials = ref<Material[]>([])
const candidates = ref<Problem[]>([])
const revisions = ref<Revision[]>([])
const board = ref<Scoreboard>()
const selected = ref('')
const revision = ref('')
const archive = ref<File>()
const archiveInput = ref<HTMLInputElement>()
const joined = ref(false)
const busy = ref(false)
const error = ref('')
const tab = ref('problems')
const editing = ref<ContestProblem>()
const published = ref<Revision[]>([])

async function run(action: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try { await action() } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Request failed'
  } finally { busy.value = false }
}

async function loadProblems() {
  problems.value = (await api<Page<ContestProblem>>(`${path}/problems`)).items
}

const editProblem = (problem: ContestProblem) => run(async () => {
  const list = await api<Page<Revision>>(`/contests/${id}/problems/${problem.id}/revisions`)
  published.value = list.items
  editing.value = { ...problem }
})

const saveProblem = () => run(async () => {
  const item = editing.value
  if (!item) return
  await api(`${path}/problems/${item.id}`, { method: 'PATCH', body: JSON.stringify({
    problem_revision_id: item.problem_revision_id,
    settings: { submission_limit: item.submission_limit || null, verdict_visibility: item.verdict_visibility },
  }) })
  editing.value = undefined
  await loadProblems()
})

onMounted(() => run(async () => {
  const [item, config] = await Promise.all([api<Contest>(path), api<Settings>(`${path}/settings`)])
  contest.value = item
  settings.value = config
  await loadProblems()
  if (item.can_manage) {
    let after: string | undefined
    do {
      const page: Page<Problem> = await api(`/problems${after ? `?after=${encodeURIComponent(after)}` : ''}`)
      candidates.value.push(...page.items)
      after = page.next
    } while (after)
  }
}))

const selectProblem = () => run(async () => {
  revision.value = ''
  revisions.value = selected.value ? (await api<Page<Revision>>(`/problems/${selected.value}/revisions`)).items.filter(r => r.status === 'published') : []
  revision.value = revisions.value[0]?.id ?? ''
})

const attach = () => run(async () => {
  if (!selected.value || !revision.value) return
  const position = Math.max(-1, ...problems.value.map(p => p.position)) + 1
  await api(`${path}/problems`, { method: 'POST', body: JSON.stringify({ problem_id: selected.value, problem_revision_id: revision.value, position, max_score: '100', verdict_visibility: 'summary' }) })
  selected.value = ''
  revision.value = ''
  revisions.value = []
  await loadProblems()
})

const importPolygon = () => run(async () => {
  if (!archive.value) return
  if (archive.value.size > 64 * 1024 * 1024) throw new Error('ZIP must not exceed 64 MiB')
  const form = new FormData()
  form.set('file', archive.value)
  await upload(`${path}/problems/import/polygon`, form)
  archive.value = undefined
  if (archiveInput.value) archiveInput.value.value = ''
  await loadProblems()
})

const saveSettings = () => run(async () => {
  const config = settings.value
  if (!config) return
  settings.value = await api<Settings>(`${path}/settings`, { method: 'PATCH', body: JSON.stringify({
    ...config,
    max_queued_submissions: config.max_queued_submissions || null,
    starts_at: config.starts_at ? new Date(config.starts_at).toISOString() : null,
    ends_at: config.ends_at ? new Date(config.ends_at).toISOString() : null,
  }) })
  if (contest.value) {
    contest.value.status = settings.value.status
    contest.value.starts_at = settings.value.starts_at ?? undefined
    contest.value.ends_at = settings.value.ends_at ?? undefined
  }
})

function localDate(value: string | null) {
  if (!value) return ''
  const date = new Date(value)
  date.setMinutes(date.getMinutes() - date.getTimezoneOffset())
  return date.toISOString().slice(0, 16)
}

const join = () => run(async () => {
  await api(`${path}/participants`, { method: 'POST', body: JSON.stringify({ self: true }) })
  joined.value = true
})

const selectTab = (name: string) => run(async () => {
  tab.value = name
  if (name === 'materials') materials.value = (await api<Page<Material>>(`${path}/materials`)).items
  if (name === 'scoreboard') board.value = await api<Scoreboard>(`${path}/scoreboard`)
})
</script>

<template>
  <section v-if="contest" class="page-head">
    <p class="eyebrow">CONTEST / {{ contest.status.toUpperCase() }}</p>
    <h1>{{ contest.title }}</h1><p>{{ contest.description }}</p>
    <p v-if="contest.starts_at">Starts {{ new Date(contest.starts_at).toLocaleString() }}</p>
    <TermButton v-if="contest.status === 'open' && settings?.self_registration" :disabled="busy || joined" @click="join">{{ joined ? 'Registered' : 'Join contest' }}</TermButton>
  </section>
  <p v-if="error" class="notice" role="alert">{{ error }}</p>
  <section v-if="contest" class="section-band">
    <div class="tab-row">
      <button :class="{selected:tab==='problems'}" :disabled="busy" @click="selectTab('problems')">Problems</button>
      <button :class="{selected:tab==='materials'}" :disabled="busy" @click="selectTab('materials')">Theory</button>
      <button :class="{selected:tab==='scoreboard'}" :disabled="busy" @click="selectTab('scoreboard')">Scoreboard</button>
      <button v-if="contest.can_manage" :class="{selected:tab==='settings'}" :disabled="busy" @click="selectTab('settings')">Settings</button>
      <button v-if="contest.can_manage" :class="{selected:tab==='participants'}" :disabled="busy" @click="selectTab('participants')">Participants</button>
      <button v-if="contest.can_manage" :class="{selected:tab==='grader'}" :disabled="busy" @click="selectTab('grader')">Grader</button>
    </div>
    <template v-if="tab === 'problems'">
      <div class="contest-list">
        <div v-for="(p,i) in problems" :key="p.id">
          <RouterLink :to="`/contests/${id}/problems/${p.id}`" class="contest-row">
            <span class="row-index">{{ String(i+1).padStart(2,'0') }}</span><strong>{{ p.title }}</strong><span>{{ p.max_score }} pts</span><span class="row-arrow">↗</span>
          </RouterLink>
          <TermButton v-if="contest.can_manage" :disabled="busy" @click="editProblem(p)">Configure {{ p.title }}</TermButton>
        </div>
        <p v-if="!problems.length" class="muted">No attached problems.</p>
      </div>
      <template v-if="contest.can_manage">
        <form v-if="editing" class="editor-form compact-form" @submit.prevent="saveProblem">
          <h2>Configure {{ editing.title }}</h2>
          <label class="term-field"><span>Pinned revision</span><select v-model="editing.problem_revision_id" :disabled="busy"><option v-for="r in published" :key="r.id" :value="r.id">#{{ r.revision_number }} · {{ r.execution_format }}</option></select></label>
          <label class="term-field"><span>Result visibility</span><select v-model="editing.verdict_visibility" :disabled="busy"><option value="hidden">Hidden</option><option value="summary">Summary</option><option value="tests">Per-test verdicts</option></select></label>
          <label class="term-field"><span>Submission limit per participant</span><input v-model.number="editing.submission_limit" type="number" min="1" :disabled="busy"></label>
          <TermButton type="submit" :disabled="busy">Save problem settings</TermButton>
          <TermButton :disabled="busy" @click="editing = undefined">Close</TermButton>
        </form>
        <form class="editor-form compact-form" @submit.prevent="attach">
          <h2>Attach existing problem</h2>
          <label class="term-field"><span>Problem</span><select v-model="selected" :disabled="busy" @change="selectProblem"><option value="">Choose problem</option><option v-for="p in candidates.filter(p => !problems.some(link => link.problem_id === p.id))" :key="p.id" :value="p.id">{{ p.title }}</option></select></label>
          <label v-if="selected" class="term-field"><span>Published revision</span><select v-model="revision" :disabled="busy"><option value="">No revision selected</option><option v-for="r in revisions" :key="r.id" :value="r.id">#{{ r.revision_number }} · {{ r.execution_format }}</option></select></label>
          <TermButton type="submit" :disabled="busy || !revision">Attach problem</TermButton>
        </form>
        <form class="editor-form compact-form" @submit.prevent="importPolygon">
          <h2>Import full Polygon ZIP</h2>
          <label class="term-field"><span>Polygon archive (up to 64 MiB)</span><input ref="archiveInput" type="file" accept=".zip,application/zip" :disabled="busy" @change="archive = ($event.target as HTMLInputElement).files?.[0]"></label>
          <TermButton type="submit" :disabled="busy || !archive">Import and attach</TermButton>
        </form>
      </template>
    </template>
    <div v-else-if="tab === 'materials'" class="material-list">
      <article v-for="m in materials" :key="m.material_revision_id" class="material-entry">
        <h3>{{ m.title }}</h3><div class="rendered-document" v-html="m.rendered_html" />
        <a v-if="m.attachment_name" :href="`/api/v1${path}/materials/${m.material_id}/attachment`">Download {{ m.attachment_name }}</a>
      </article>
      <p v-if="!materials.length" class="muted">No materials attached.</p>
      <MaterialEditor v-if="contest.can_manage" :contest="id" :position="materials.length" @saved="selectTab('materials')" />
    </div>
    <div v-else-if="tab === 'scoreboard' && board" class="table-scroll">
      <table><thead><tr><th>Participant</th><th v-for="p in board.problems" :key="p.id">{{ p.title }}</th><th>Total</th></tr></thead>
      <tbody><tr v-for="row in board.rows" :key="row.participant_id"><td>{{ board.participants.find(p => p.id === row.participant_id)?.display_name }}</td><td v-for="p in board.problems" :key="p.id">{{ row.scores[p.id] }}</td><td>{{ row.total }}</td></tr></tbody></table>
    </div>
    <form v-else-if="tab === 'settings' && settings && contest.can_manage" class="editor-form compact-form" @submit.prevent="saveSettings">
      <label class="term-field"><span>Status</span><select v-model="settings.status" :disabled="busy"><option value="draft">Draft</option><option value="open">Open</option><option value="closed">Closed</option></select></label>
      <label><input v-model="settings.self_registration" type="checkbox" :disabled="busy"> Allow self-registration</label>
      <label class="term-field"><span>Starts</span><input type="datetime-local" :value="localDate(settings.starts_at)" :disabled="busy" @change="settings.starts_at = ($event.target as HTMLInputElement).value || null"></label>
      <label class="term-field"><span>Ends</span><input type="datetime-local" :value="localDate(settings.ends_at)" :disabled="busy" @change="settings.ends_at = ($event.target as HTMLInputElement).value || null"></label>
      <label class="term-field"><span>Scoreboard visibility</span><select v-model="settings.scoreboard_visibility" :disabled="busy"><option value="private">Managers</option><option value="contest">Contest</option></select></label>
      <label class="term-field"><span>Maximum queued submissions</span><input v-model.number="settings.max_queued_submissions" type="number" min="1" :disabled="busy"></label>
      <TermButton type="submit" :disabled="busy">Save settings</TermButton>
    </form>
    <template v-else-if="tab === 'participants' && contest.can_manage">
      <ResourceMembers :path="`${path}/participants`" />
      <ResourceMembers :path="`${path}/members`" :roles="['manager', 'viewer']" />
    </template>
    <GraderManager v-else-if="tab === 'grader' && contest.can_manage" :contest="id" :problems="problems" />
  </section>
</template>
