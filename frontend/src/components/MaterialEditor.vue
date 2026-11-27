<script setup lang="ts">
import { ref } from 'vue'
import { api } from '../api/client'
import type { Page } from '../api/types'
import TermButton from './termui/TermButton.vue'

type Revision = { id: string; revision_number: number; source?: string; format: string; rendered_html: string }
const emit = defineEmits<{ saved: [] }>()
const props = defineProps<{ contest: string; position: number }>()
const id = ref('')
const title = ref('')
const source = ref('')
const file = ref<File>()
const fileInput = ref<HTMLInputElement>()
const format = ref('markdown')
const preview = ref('')
const visibility = ref('contest')
const history = ref<Revision[]>([])
const selected = ref('')
const busy = ref(false)
const error = ref('')

async function run(action: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try { await action() } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Could not update material'
  } finally { busy.value = false }
}

const body = () => JSON.stringify({ format: format.value, source: source.value })
const revisions = () => `/materials/${encodeURIComponent(id.value.trim())}/revisions`
const load = () => run(async () => {
  history.value = (await api<Page<Revision>>(revisions())).items ?? []
  selected.value = history.value[0]?.id ?? ''
})
const render = () => run(async () => {
  preview.value = (await api<{ html: string }>('/materials/render', { method: 'POST', body: body() })).html
})
const save = () => run(async () => {
  if (!id.value.trim()) {
    const material = await api<{ id: string }>('/materials', { method: 'POST', body: JSON.stringify({ title: title.value }) })
    id.value = material.id
  }
  let payload: string | FormData = body()
  if (file.value) {
    const form = new FormData()
    form.set('format', format.value)
    form.set('source', source.value)
    form.set('file', file.value)
    payload = form
  }
  const revision = await api<Revision>(revisions(), { method: 'POST', body: payload })
  file.value = undefined
  if (fileInput.value) fileInput.value.value = ''
  history.value = [revision, ...history.value]
  selected.value = revision.id
  preview.value = revision.rendered_html
})
const attach = () => run(async () => {
  await api(`/contests/${props.contest}/materials`, { method: 'POST', body: JSON.stringify({
    material_id: id.value.trim(), material_revision_id: selected.value, position: props.position, visibility: visibility.value,
  }) })
  emit('saved')
})
function restore() {
  const revision = history.value.find(item => item.id === selected.value)
  if (!revision) return
  preview.value = revision.rendered_html
  if (revision.source === undefined) return
  source.value = revision.source
  format.value = revision.format
}
</script>

<template>
  <form class="editor-form compact-form" @submit.prevent="save">
    <h2>Theory editor</h2>
    <p v-if="error" class="notice" role="alert">{{ error }}</p>
    <label class="term-field"><span>Existing material ID</span><input v-model="id" :disabled="busy" @input="history = []; selected = ''"></label>
    <TermButton :disabled="busy || !id.trim()" @click="load">Load revision history</TermButton>
    <label v-if="!id.trim()" class="term-field"><span>New material title</span><input v-model="title" required :disabled="busy"></label>
    <label class="term-field"><span>Format</span><select v-model="format" :disabled="busy"><option value="markdown">Markdown</option><option value="latex">LaTeX</option></select></label>
    <label class="term-field"><span>Source</span><textarea v-model="source" rows="10" :disabled="busy" /></label>
    <label class="term-field"><span>Optional attachment (up to 8 MiB)</span><input ref="fileInput" type="file" :disabled="busy" @change="file = ($event.target as HTMLInputElement).files?.[0]"></label>
    <TermButton :disabled="busy || !source.trim()" @click="render">Preview</TermButton>
    <TermButton type="submit" :disabled="busy || !source.trim() || (!id.trim() && !title.trim()) || (file?.size ?? 0) > 8 * 1024 * 1024">Save revision</TermButton>
    <article v-if="preview" class="rendered-document" v-html="preview" />
    <template v-if="history.length">
      <label class="term-field"><span>Revision history</span><select v-model="selected" :disabled="busy" @change="restore"><option v-for="revision in history" :key="revision.id" :value="revision.id">#{{ revision.revision_number }}</option></select></label>
      <label class="term-field"><span>Visibility</span><select v-model="visibility" :disabled="busy"><option value="contest">Contest</option><option value="managers">Managers</option></select></label>
      <TermButton :disabled="busy || !selected" @click="attach">Pin selected revision</TermButton>
    </template>
  </form>
</template>
