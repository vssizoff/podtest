import { defineStore } from 'pinia'
import { api, setCSRF } from '../api/client'
import type { Session, User } from '../api/types'

export const useSession = defineStore('session', {
  state: () => ({ user: null as User | null, loaded: false }),
  actions: {
    async load() { try { const session = await api<Session>('/auth/session'); this.user = session.user ?? null; setCSRF(session.csrf_token ?? '') } finally { this.loaded = true } },
    async logout() { await api<void>('/auth/logout', { method: 'POST' }); this.user = null; setCSRF('') },
  },
})
