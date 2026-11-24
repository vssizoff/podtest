import { createRouter, createWebHistory } from 'vue-router'
import { useSession } from './state/session'
import AppShell from './views/AppShell.vue'
import Overview from './views/Overview.vue'
import Collection from './views/Collection.vue'
import ContestView from './views/ContestView.vue'
import ProblemView from './views/ProblemView.vue'
import Authoring from './views/Authoring.vue'
import CourseDetail from './views/CourseDetail.vue'
import ProblemDetail from './views/ProblemDetail.vue'

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: '/', component: AppShell, children: [
      { path: '', component: Overview },
      { path: 'courses', component: Collection, props: { kind: 'course' } },
      { path: 'contests', component: Collection, props: { kind: 'contest' } },
      { path: 'problems', component: Collection, props: { kind: 'problem' } },
      { path: 'courses/:id', component: CourseDetail },
      { path: 'problems/:id', component: ProblemDetail },
      { path: 'contests/:id', component: ContestView },
      { path: 'contests/:id/problems/:problemId', component: ProblemView },
      { path: 'authoring', component: Authoring },
    ] },
  ],
})

router.beforeEach(async to => {
  const session = useSession()
  if (!session.loaded) await session.load().catch(() => undefined)
  if (!session.user && to.path !== '/') return { path: '/' }
})
