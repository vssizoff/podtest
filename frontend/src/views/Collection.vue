<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { api } from '../api/client'
import type { Course, Contest, Problem, Page } from '../api/types'
const props = defineProps<{ kind: 'course' | 'contest' | 'problem' }>()
const items = ref<(Course | Contest | Problem)[]>([])
const loading = ref(true); const error = ref('')
const singular = computed(() => props.kind[0].toUpperCase() + props.kind.slice(1))
async function load() { loading.value = true; error.value = ''; try { const data = await api<Page<Course | Contest | Problem>>(`/${props.kind}s`); items.value = data.items } catch (e) { error.value = e instanceof Error ? e.message : 'Could not load items' } finally { loading.value = false } }
watch(() => props.kind, load); onMounted(load)
</script>
<template>
  <section class="page-head"><p class="eyebrow">CATALOG / {{ kind.toUpperCase() }}</p><h1>{{ singular }}s</h1><p>Browse {{ kind }} content available to your account.</p></section>
  <p v-if="error" class="notice">{{ error }}</p><p v-else-if="loading" class="muted">Loading {{ kind }}s…</p>
  <div v-else-if="!items.length" class="empty-state"><span class="empty-symbol">○</span><div><h3>No {{ kind }}s found</h3><p>There are no items available in this catalog.</p></div></div>
  <div v-else class="catalog-list"><RouterLink v-for="item in items" :key="item.id" :to="`/${kind}s/${item.id}`" class="catalog-row"><span class="catalog-icon">{{ kind === 'course' ? 'C' : kind === 'contest' ? 'T' : 'P' }}</span><div><strong>{{ item.title }}</strong><p>{{ item.description || 'No description' }}</p></div><span class="status">{{ 'status' in item ? item.status : item.can_manage ? 'MANAGED' : 'AVAILABLE' }}</span><span>↗</span></RouterLink></div>
</template>
