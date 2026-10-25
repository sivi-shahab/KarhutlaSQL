<script setup>
import { ref } from 'vue'
import { login, session } from '../api.js'

const emit = defineEmits(['done'])
const mode = ref('password')
const user = ref('')
const password = ref('')
const token = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  busy.value = true
  try {
    if (mode.value === 'password') {
      await login(user.value, password.value)
    } else {
      session.token = token.value.trim()
    }
    password.value = ''
    emit('done')
  } catch (e) {
    error.value = e.message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <main class="login">
    <form class="card" @submit.prevent="submit">
      <div class="brand big"><img src="/logo.svg" alt="" width="40" height="40" /> KarhutlaSQL</div>
      <div class="seg">
        <button type="button" :class="{ active: mode === 'password' }" @click="mode = 'password'">Password</button>
        <button type="button" :class="{ active: mode === 'token' }" @click="mode = 'token'">SSO token</button>
      </div>
      <template v-if="mode === 'password'">
        <label>User <input v-model="user" autocomplete="username" required autofocus /></label>
        <label>Password <input v-model="password" type="password" autocomplete="current-password" required /></label>
      </template>
      <label v-else>Access token
        <textarea v-model="token" rows="4" required placeholder="paste an OIDC access token" />
      </label>
      <button class="primary" :disabled="busy">{{ busy ? 'Signing in…' : 'Sign in' }}</button>
      <p v-if="error" class="error" role="alert">{{ error }}</p>
      <p class="hint">Every query runs with your own privileges, masking, and audit trail.</p>
    </form>
  </main>
</template>
