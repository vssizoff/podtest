<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { api, upload } from '../api/client'
import TermButton from '../components/termui/TermButton.vue'

type Report = { title: string; warnings: string[] }
const file = ref<File>()
const report = ref<Report>()
const error = ref('')
const busy = ref(false)
const router = useRouter()
const kind = ref('courses')
const title = ref('')
const description = ref('')
const createError = ref('')
const creating = ref(false)

async function create() {
  if (creating.value || !title.value.trim()) return
  creating.value = true
  createError.value = ''
  const collection = kind.value
  try {
    const { id } = await api<{ id: string }>(`/${collection}`, { method: 'POST', body: JSON.stringify({ title: title.value, description: description.value }) })
    await router.push(`/${collection}/${id}`)
  } catch (cause) {
    createError.value = cause instanceof Error ? cause.message : 'Could not create resource'
  } finally { creating.value = false }
}

async function inspect() {
  if (!file.value || busy.value) return
  busy.value = true
  error.value = ''
  report.value = undefined
  try {
    const body = new FormData()
    body.set('file', file.value)
    report.value = await upload<Report>('/problems/import/polygon', body)
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Could not inspect package'
  } finally { busy.value = false }
}
</script>

<template>
  <section class="page-head">
    <p class="eyebrow">AUTHOR WORKSPACE</p><h1>Problem studio</h1>
    <p>Create courses, contests and problems here. Use their detail pages to configure them.</p>
  </section>
  <form class="editor-form compact-form" @submit.prevent="create">
    <h2>Create</h2>
    <label class="term-field"><span>Resource type</span><select v-model="kind" :disabled="creating"><option value="courses">Course</option><option value="contests">Contest</option><option value="problems">Problem</option></select></label>
    <label class="term-field"><span>Title</span><input v-model="title" maxlength="200" required :disabled="creating"></label>
    <label class="term-field"><span>Description</span><textarea v-model="description" rows="3" :disabled="creating" /></label>
    <TermButton variant="primary" type="submit" :disabled="creating || !title.trim()">Create</TermButton>
    <p v-if="createError" class="notice" role="alert">{{ createError }}</p>
  </form>
  <form class="editor-form compact-form" @submit.prevent="inspect">
    <h2>Inspect Polygon ZIP</h2>
    <p>This checks package metadata only. It does not create a problem or an executable revision.</p>
    <input type="file" aria-label="Polygon ZIP" accept=".zip,application/zip" :disabled="busy" @change="file = ($event.target as HTMLInputElement).files?.[0]">
    <TermButton type="submit" :disabled="!file || busy">{{ busy ? 'Inspecting…' : 'Inspect ZIP' }}</TermButton>
    <p v-if="error" class="notice" role="alert">{{ error }}</p>
    <div v-if="report" class="notice" role="status"><strong>{{ report.title }}</strong><ul><li v-for="warning in report.warnings" :key="warning">{{ warning }}</li></ul></div>
  </form>
</template>
