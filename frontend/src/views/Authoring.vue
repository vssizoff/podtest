<script setup lang="ts">
import { ref } from 'vue'
import { RouterLink } from 'vue-router'
import { upload } from '../api/client'
import TermButton from '../components/termui/TermButton.vue'

type Report = { title: string; warnings: string[] }
const file = ref<File>()
const report = ref<Report>()
const error = ref('')
const busy = ref(false)

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
    <p>Create a problem in the catalog, then use its detail page to edit and publish a scripted Lua revision.</p>
    <RouterLink class="primary-link" to="/problems">Open Problems →</RouterLink>
  </section>
  <form class="editor-form compact-form" @submit.prevent="inspect">
    <h2>Inspect Polygon ZIP</h2>
    <p>This checks package metadata only. It does not create a problem or an executable revision.</p>
    <input type="file" aria-label="Polygon ZIP" accept=".zip,application/zip" :disabled="busy" @change="file = ($event.target as HTMLInputElement).files?.[0]">
    <TermButton type="submit" :disabled="!file || busy">{{ busy ? 'Inspecting…' : 'Inspect ZIP' }}</TermButton>
    <p v-if="error" class="notice" role="alert">{{ error }}</p>
    <div v-if="report" class="notice" role="status"><strong>{{ report.title }}</strong><ul><li v-for="warning in report.warnings" :key="warning">{{ warning }}</li></ul></div>
  </form>
</template>
