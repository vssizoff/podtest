<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { api } from '../api/client'
import type { Page } from '../api/types'
import TermButton from './termui/TermButton.vue'

const props = defineProps<{ path: string; roles?: string[] }>()
type Member = { user_id: string; display_name: string; role?: string; status?: string }
const items = ref<Member[]>([])
const user = ref('')
const role = ref(props.roles?.[0] ?? '')
const busy = ref(false)
const error = ref('')

async function run(action: () => Promise<void>) {
  if (busy.value) return
  busy.value = true
  error.value = ''
  try {
    await action()
    items.value = (await api<Page<Member>>(props.path)).items ?? []
  } catch (cause) {
    error.value = cause instanceof Error ? cause.message : 'Could not update access'
  } finally { busy.value = false }
}

onMounted(() => run(async () => {}))

const add = () => run(async () => {
  const id = user.value.trim()
  if (!id) return
  if (props.roles) await api(`${props.path}/${encodeURIComponent(id)}/${role.value}`, { method: 'PUT' })
  else await api(props.path, { method: 'POST', body: JSON.stringify({ user_id: id }) })
  user.value = ''
})

const remove = (item: Member) => run(async () => {
  await api(`${props.path}/${encodeURIComponent(item.user_id)}${item.role ? `/${item.role}` : ''}`, { method: 'DELETE' })
})
</script>

<template>
  <section class="editor-form compact-form">
    <h2>{{ roles ? 'Roles' : 'Participants' }}</h2>
    <p v-if="error" class="notice" role="alert">{{ error }}</p>
    <form class="editor-form" @submit.prevent="add">
      <label class="term-field"><span>User ID</span><input v-model="user" required :disabled="busy"></label>
      <label v-if="roles" class="term-field"><span>Role</span><select v-model="role" :disabled="busy"><option v-for="option in roles" :key="option">{{ option }}</option></select></label>
      <TermButton type="submit" :disabled="busy || !user.trim()">{{ roles ? 'Grant role' : 'Register participant' }}</TermButton>
    </form>
    <div v-for="item in items" :key="`${item.user_id}/${item.role ?? ''}`" class="catalog-row">
      <span>{{ item.display_name }}</span><span>{{ item.role ?? item.status }}</span>
      <TermButton :disabled="busy || item.status === 'removed'" @click="remove(item)">Remove</TermButton>
    </div>
  </section>
</template>
