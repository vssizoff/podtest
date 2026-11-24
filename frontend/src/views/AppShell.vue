<script setup lang="ts">
import { RouterLink, RouterView, useRouter } from 'vue-router'
import { computed } from 'vue'
import { useSession } from '../state/session'
import TermButton from '../components/termui/TermButton.vue'
const session = useSession()
const router = useRouter()
const initials = computed(() => (session.user?.display_name ?? 'U').split(/\s+/).map(x => x[0]).slice(0, 2).join('').toUpperCase())
async function logout() { await session.logout(); await router.push('/') }
</script>
<template>
  <div class="app-frame">
    <header class="topbar">
      <RouterLink to="/" class="wordmark"><span class="mark">P</span><span>PODTEST</span></RouterLink>
      <nav><RouterLink to="/courses">Courses</RouterLink><RouterLink to="/contests">Contests</RouterLink><RouterLink to="/problems">Problems</RouterLink><RouterLink to="/authoring">Workspace</RouterLink></nav>
      <div v-if="session.user" class="account"><span class="avatar">{{ initials }}</span><span class="account-name">{{ session.user.display_name }}</span><TermButton @click="logout">Sign out</TermButton></div>
      <a v-else class="login-link" href="/auth/login">Sign in <span>↗</span></a>
    </header>
    <main><RouterView :key="$route.path" /></main>
    <footer><span>Podtest · programming laboratory</span><span>BUILD / VERIFY / REVIEW</span></footer>
  </div>
</template>
