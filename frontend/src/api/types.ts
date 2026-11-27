export type User = { id: string; display_name: string; email: string; avatar_url: string }
export type Session = { authenticated: boolean; user?: User; csrf_token?: string }
export type Course = { id: string; title: string; description: string; created_at: string; can_manage: boolean }
export type Contest = Course & { status: 'draft' | 'open' | 'closed'; starts_at?: string; ends_at?: string }
export type Problem = { id: string; title: string; description: string; created_at: string; can_manage: boolean }
export type ContestProblem = { id: string; problem_id: string; problem_revision_id: string; title: string; position: number; max_score: string; submission_limit: number | null; verdict_visibility: 'hidden' | 'summary' | 'tests'; grader_mode?: string }
export type Statement = { locale: string; rendered_html: string; digest: string; renderer_version: string }
export type Submission = { id: string; contest_id: string; contest_problem_id: string; problem_revision_id: string; submitted_at: string; language: string }
export type Attempt = {
  id: string
  attempt_number: number
  status: string
  verdict?: string
  base_score?: string
  phase?: 'building' | 'running' | 'testing'
  active_tests?: string[]
  tests?: { test_id: string; verdict: string; score?: string | null; checker_metadata?: Record<string, unknown> }[]
  protocol?: { problem_revision_id: string; started_at?: string; finished_at?: string; groups: { subgroup_id: string; score: string }[] | null }
  artifacts?: { id: string; kind: string }[]
  can_cancel?: boolean
  cancel_pending?: boolean
}
export type Material = { material_id: string; material_revision_id: string; position: number; visibility: string; title: string; format: string; rendered_html: string; attachment_name?: string }
export type Scoreboard = { contest_id: string; problems: { id: string; title: string; max_score: string }[]; participants: { id: string; user_id: string; display_name: string }[]; rows: { participant_id: string; rank: number; total: string; scores: Record<string, string> }[] }
export type Page<T> = { items: T[]; next?: string }
