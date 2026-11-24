<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink, useRouter } from 'vue-router'
import { api } from '../api/client'
import type { Course, Contest, Problem, Page } from '../api/types'
import TermButton from '../components/termui/TermButton.vue'
import TermInput from '../components/termui/TermInput.vue'
const props = defineProps<{ kind: 'course' | 'contest' | 'problem' }>()
const router = useRouter()
const items = ref<(Course | Contest | Problem)[]>([])
const loading = ref(true); const error = ref(''); const creating = ref(false); const title = ref(''); const description = ref('')
const singular = computed(() => props.kind[0].toUpperCase() + props.kind.slice(1))
async function load() { loading.value = true; error.value = ''; try { const data = await api<Page<Course | Contest | Problem>>(`/${props.kind}s`); items.value = data.items } catch (e) { error.value = e instanceof Error ? e.message : 'Could not load items' } finally { loading.value = false } }
async function create() { if (!title.value.trim()) return; try { const item = await api<Course>(`/${props.kind}s`, { method: 'POST', body: JSON.stringify({ title: title.value, description: description.value }) }); await router.push(`/${props.kind}s/${item.id}`) } catch (e) { error.value = e instanceof Error ? e.message : 'Could not create' } }
watch(() => props.kind, load); onMounted(load)
</script>
<template>
  <section class="page-head"><p class="eyebrow">CATALOG / {{ kind.toUpperCase() }}</p><div class="heading-line"><h1>{{ singular }}s</h1><TermButton variant="primary" @click="creating = !creating">{{ creating ? 'Close' : `New ${singular.toLowerCase()}` }}</TermButton></div><p>Explore and manage {{ kind }} content available to your account.</p></section>
  <form v-if="creating" class="editor-form compact-form" @submit.prevent="create"><TermInput v-model="title" label="Title" placeholder="Name"/><TermInput v-model="description" label="Description" :rows="3" placeholder="Optional overview"/><TermButton type="submit" variant="primary">Create {{ singular.toLowerCase() }}</TermButton></form>
  <p v-if="error" class="notice">{{ error }}</p><p v-else-if="loading" class="muted">Loading {{ kind }}s…</p>
  <div v-else-if="!items.length" class="empty-state"><span class="empty-symbol">○</span><div><h3>No {{ kind }}s found</h3><p>There are no items available in this catalog.</p></div></div>
  <div v-else class="catalog-list"><RouterLink v-for="item in items" :key="item.id" :to="`/${kind}s/${item.id}`" class="catalog-row"><span class="catalog-icon">{{ kind === 'course' ? 'C' : kind === 'contest' ? 'T' : 'P' }}</span><div><strong>{{ item.title }}</strong><p>{{ item.description || 'No description' }}</p></div><span class="status">{{ 'status' in item ? item.status : item.can_manage ? 'MANAGED' : 'AVAILABLE' }}</span><span>↗</span></RouterLink></div>
</template>
