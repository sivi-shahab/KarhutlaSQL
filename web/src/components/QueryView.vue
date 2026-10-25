<script setup>
import { computed, onMounted, ref } from 'vue'
import { ApiError, query, tables } from '../api.js'
import ResultTable from './ResultTable.vue'

const emit = defineEmits(['unauthorized'])
const sql = ref('SELECT 42 AS answer')
const result = ref(null)
const error = ref('')
const running = ref(false)
const list = ref([])
const filter = ref('')
let abort = null

const grouped = computed(() => {
  const f = filter.value.toLowerCase()
  const out = {}
  for (const t of list.value) {
    const key = `${t.catalog}.${t.schema}`
    if (f && !`${key}.${t.name}`.toLowerCase().includes(f)) continue
    ;(out[key] ||= []).push(t)
  }
  return out
})

function handle(e) {
  if (e instanceof ApiError && e.status === 401) emit('unauthorized')
  else if (e.name !== 'AbortError') error.value = e.message
}

async function loadTables() {
  try { list.value = await tables() } catch (e) { handle(e) }
}

async function run() {
  if (running.value) return
  error.value = ''
  running.value = true
  abort = new AbortController()
  try {
    result.value = await query(sql.value, [], abort.signal)
  } catch (e) {
    result.value = null
    handle(e)
  } finally {
    running.value = false
    abort = null
  }
}

function cancel() { abort?.abort() } // menutup koneksi → statement dibatalkan di server

function pick(t) {
  sql.value = `SELECT * FROM ${t.catalog}.${t.schema}.${t.name} LIMIT 100`
  run()
}

function keydown(e) {
  if ((e.ctrlKey || e.metaKey) && e.key === 'Enter') {
    e.preventDefault()
    run()
  }
}

onMounted(loadTables)
</script>

<template>
  <div class="workspace">
    <aside class="sidebar">
      <div class="side-head">
        <strong>Tables</strong>
        <button class="ghost small" title="Refresh" @click="loadTables">↻</button>
      </div>
      <input v-model="filter" class="filter" placeholder="Filter tables" />
      <div v-if="!list.length" class="muted pad">No readable tables.</div>
      <details v-for="(ts, key) in grouped" :key="key" open>
        <summary>{{ key }}</summary>
        <button v-for="t in ts" :key="t.name" class="table-item" :title="t.type" @click="pick(t)">{{ t.name }}</button>
      </details>
    </aside>
    <section class="main">
      <div class="editor">
        <textarea v-model="sql" spellcheck="false" aria-label="SQL" @keydown="keydown" />
        <div class="actions">
          <button class="primary" :disabled="running" @click="run">{{ running ? 'Running…' : 'Run' }}</button>
          <button v-if="running" class="ghost" @click="cancel">Cancel</button>
          <span class="muted">Ctrl/⌘ + Enter</span>
        </div>
      </div>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <ResultTable v-if="result" :result="result" />
    </section>
  </div>
</template>
