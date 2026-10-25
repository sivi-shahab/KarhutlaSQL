<script setup>
import { onMounted, ref } from 'vue'
import { ApiError, query } from '../api.js'
import ResultTable from './ResultTable.vue'

const props = defineProps({ view: String })
const emit = defineEmits(['unauthorized'])
const result = ref(null)
const error = ref('')
const notice = ref('')

// Fungsi karhutla_* menegakkan izinnya sendiri (admin melihat semua,
// user lain hanya miliknya; karhutla_jobs khusus admin).
const views = {
  sessions: {
    title: 'Sessions',
    sql: 'SELECT session_id, user, source, peer, started_at, last_active_at, active_statements, statement, current FROM karhutla_sessions() ORDER BY started_at DESC',
  },
  history: {
    title: 'Query history',
    sql: 'SELECT started_at, user, kind, resource_group, status, queue_ms, duration_ms, rows, statement, error FROM karhutla_query_history() ORDER BY started_at DESC LIMIT 500',
  },
  jobs: {
    title: 'Scheduled jobs',
    sql: 'SELECT name, kind, target, schedule, next_run, running, last_start, last_end, last_status, last_rows, last_error FROM karhutla_jobs()',
  },
}

const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/

function handle(e) {
  if (e instanceof ApiError && e.status === 401) emit('unauthorized')
  else error.value = e.message
}

async function load() {
  error.value = ''
  try { result.value = await query(views[props.view].sql) } catch (e) { result.value = null; handle(e) }
}

async function kill(id) {
  if (!uuid.test(id) || !confirm(`Kill session ${id}?`)) return
  try {
    await query(`CALL karhutla_kill_session('${id}')`)
    notice.value = `Session ${id} killed.`
    await load()
  } catch (e) { handle(e) }
}

async function runJob(name) {
  if (!/^[a-z0-9_]+$/.test(name)) return
  try {
    await query(`CALL karhutla_run_job('${name}')`)
    notice.value = `Job ${name} started.`
    setTimeout(load, 1000)
  } catch (e) { handle(e) }
}

onMounted(load)
</script>

<template>
  <section class="main padded">
    <div class="side-head">
      <h2>{{ views[view].title }}</h2>
      <button class="ghost small" @click="load">Refresh</button>
    </div>
    <p v-if="notice" class="notice">{{ notice }}</p>
    <p v-if="error" class="error" role="alert">{{ error }}</p>
    <div v-if="result && view === 'sessions'" class="row-actions">
      <span class="muted">Kill a session (admin):</span>
      <button v-for="r in result.rows.filter((r) => r[8] !== 'true')" :key="r[0]" class="ghost small" @click="kill(r[0])">
        {{ r[1] }} · {{ r[0].slice(0, 8) }}
      </button>
    </div>
    <div v-if="result && view === 'jobs' && result.rows.length" class="row-actions">
      <span class="muted">Run now (admin):</span>
      <button v-for="r in result.rows" :key="r[0]" class="ghost small" @click="runJob(r[0])">{{ r[0] }}</button>
    </div>
    <ResultTable v-if="result" :result="result" :limit="500" />
  </section>
</template>
