<script setup>
import { ref } from 'vue'
import { session } from './api.js'
import Login from './components/Login.vue'
import QueryView from './components/QueryView.vue'
import AdminView from './components/AdminView.vue'

const authed = ref(!!session.token)
const tab = ref('query')
const tabs = [
  { id: 'query', label: 'Query' },
  { id: 'sessions', label: 'Sessions' },
  { id: 'history', label: 'Query history' },
  { id: 'jobs', label: 'Jobs' },
]

function logout() {
  session.token = null
  authed.value = false
}
</script>

<template>
  <Login v-if="!authed" @done="authed = true" />
  <div v-else class="shell">
    <header class="topbar">
      <div class="brand"><img src="/logo.svg" alt="" width="26" height="26" /> KarhutlaSQL</div>
      <nav>
        <button v-for="t in tabs" :key="t.id" :class="{ active: tab === t.id }" @click="tab = t.id">{{ t.label }}</button>
      </nav>
      <button class="ghost" @click="logout">Sign out</button>
    </header>
    <QueryView v-if="tab === 'query'" @unauthorized="logout" />
    <AdminView v-else :view="tab" :key="tab" @unauthorized="logout" />
  </div>
</template>
