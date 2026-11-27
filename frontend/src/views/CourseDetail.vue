<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { api } from '../api/client'
import type { Contest, Course, Page } from '../api/types'
import TermButton from '../components/termui/TermButton.vue'
import ResourceMembers from '../components/ResourceMembers.vue'

const path = `/courses/${String(useRoute().params.id)}`
const course = ref<Course>()
const contests = ref<Contest[]>([])
const choices = ref<Contest[]>([])
const selected = ref('')
const error = ref('')
const busy = ref(true)

async function loadContests() {
  contests.value = (await api<Page<Contest>>(`${path}/contests`)).items
}

onMounted(async () => {
  try {
    course.value = await api<Course>(path)
    await loadContests()
    if (course.value.can_manage) {
      let cursor: string | undefined
      do {
        const page: Page<Contest> = await api(`/contests${cursor ? `?after=${encodeURIComponent(cursor)}` : ''}`)
        choices.value.push(...page.items.filter(item => item.can_manage))
        cursor = page.next
      } while (cursor)
    }
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Could not load course'
  } finally { busy.value = false }
})

async function attach() {
  if (!selected.value || busy.value) return
  busy.value = true
  error.value = ''
  try {
    await api(`${path}/contests`, { method: 'POST', body: JSON.stringify({ contest_id: selected.value, position: contests.value.length }) })
    selected.value = ''
    await loadContests()
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Could not link contest'
  } finally { busy.value = false }
}
</script>

<template>
  <RouterLink to="/courses">← Courses</RouterLink>
  <section v-if="course" class="page-head">
    <p class="eyebrow">COURSE</p><h1>{{ course.title }}</h1><p>{{ course.description }}</p>
  </section>
  <p v-if="error" class="notice" role="alert">{{ error }}</p>
  <p v-if="busy" class="muted">Loading…</p>
  <section v-if="course" class="section-band">
    <h2>Contests</h2>
    <div v-if="contests.length" class="catalog-list">
      <RouterLink v-for="contest in contests" :key="contest.id" :to="`/contests/${contest.id}`" class="catalog-row">
        <span class="catalog-icon">T</span><div><strong>{{ contest.title }}</strong><p>{{ contest.description }}</p></div><span class="status">{{ contest.status }}</span><span>↗</span>
      </RouterLink>
    </div>
    <p v-else-if="!busy" class="muted">No visible contests linked to this course.</p>
    <form v-if="course.can_manage" class="editor-form compact-form" @submit.prevent="attach">
      <label class="term-field"><span>Link an existing contest</span>
        <select v-model="selected" :disabled="busy"><option value="">Choose a contest</option><option v-for="contest in choices.filter(c => !contests.some(link => link.id === c.id))" :key="contest.id" :value="contest.id">{{ contest.title }}</option></select>
      </label>
      <TermButton type="submit" :disabled="busy || !selected">Link contest</TermButton>
    </form>
  </section>
  <ResourceMembers v-if="course?.can_manage" :path="`${path}/members`" :roles="['manager', 'teacher', 'student', 'viewer']" />
</template>
