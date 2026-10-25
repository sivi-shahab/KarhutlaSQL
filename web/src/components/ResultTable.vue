<script setup>
import { computed } from 'vue'

const props = defineProps({ result: Object, limit: { type: Number, default: 1000 } })
const shown = computed(() => props.result.rows.slice(0, props.limit))

function csvCell(v) {
  if (v === null) return ''
  return /[",\n]/.test(v) ? '"' + v.replaceAll('"', '""') + '"' : v
}

function download() {
  const r = props.result
  const lines = [r.columns.map((c) => csvCell(c.name)).join(',')]
  for (const row of r.rows) lines.push(row.map(csvCell).join(','))
  const url = URL.createObjectURL(new Blob([lines.join('\n') + '\n'], { type: 'text/csv' }))
  const a = Object.assign(document.createElement('a'), { href: url, download: 'result.csv' })
  a.click()
  URL.revokeObjectURL(url)
}
</script>

<template>
  <div class="result">
    <div class="meta">
      <span>{{ result.rowCount.toLocaleString() }} row{{ result.rowCount === 1 ? '' : 's' }} · {{ result.ms }} ms</span>
      <span v-if="result.rowCount > limit" class="muted">showing first {{ limit.toLocaleString() }}</span>
      <button class="ghost small" @click="download">Download CSV</button>
    </div>
    <div class="grid">
      <table>
        <thead>
          <tr><th v-for="c in result.columns" :key="c.name" :title="c.type">{{ c.name }}</th></tr>
        </thead>
        <tbody>
          <tr v-for="(row, i) in shown" :key="i">
            <td v-for="(v, j) in row" :key="j" :class="{ null: v === null }">{{ v === null ? 'NULL' : v }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
