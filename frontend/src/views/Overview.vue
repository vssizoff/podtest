<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { api } from '../api/client'
import type { Contest, Page } from '../api/types'
import { useSession } from '../state/session'
import { RouterLink } from 'vue-router'
const session = useSession()
const contests = ref<Contest[]>([])
const failed = ref(false)
onMounted(async () => { try { const data = await api<Page<Contest>>('/contests'); contests.value = data.items } catch { failed.value = true } })
</script>
<template>
  <section class="welcome">
    <div class="welcome-copy"><p class="eyebrow"><span class="live-dot"></span> CONTEST SYSTEM / 01</p><h1>Think in<br><span>problems.</span></h1><p class="intro">{{ session.user ? `Welcome back, ${session.user.display_name}.` : 'A focused workspace for programming courses, contests, and careful evaluation.' }}</p><a v-if="!session.user" class="primary-link" href="/auth/login">Continue with your account <span>↗</span></a><RouterLink v-else class="primary-link" to="/contests">Open contests <span>→</span></RouterLink></div>
    <div class="overview-art" aria-hidden="true"><div class="art-grid"></div><div class="orbit orbit-one"></div><div class="orbit orbit-two"></div><div class="art-core"><span>RUN</span><strong>01</strong><span>READY</span></div><div class="art-label art-label-a">runtime<br>isolated</div><div class="art-label art-label-b">revision<br>pinned</div></div>
  </section>
  <section class="section-band">
    <div class="section-heading"><div><p class="eyebrow">CURRENT ACTIVITY</p><h2>Contests</h2></div><RouterLink class="text-link" to="/contests">Browse all <span>→</span></RouterLink></div>
    <div v-if="failed" class="notice">Contest data is temporarily unavailable.</div>
    <div v-else-if="!contests.length" class="empty-state"><span class="empty-symbol">○</span><div><h3>No contests yet</h3><p>New contests will appear here once they are available.</p></div></div>
    <div v-else class="contest-list"><RouterLink v-for="(contest, i) in contests.slice(0, 4)" :key="contest.id" :to="`/contests/${contest.id}`" class="contest-row"><span class="row-index">{{ String(i + 1).padStart(2, '0') }}</span><strong>{{ contest.title }}</strong><span>{{ contest.status }}</span><span class="row-arrow">↗</span></RouterLink></div>
  </section>
</template>
