<script setup lang="ts">
import { ref } from 'vue'
import Button from 'primevue/button'
import Card from 'primevue/card'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import Password from 'primevue/password'
import { ApiError, login } from '../api/auth'

const username = ref('')
const password = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  busy.value = true
  try {
    await login(username.value, password.value)
    location.reload() // the app now starts as a signed-in user
  } catch (e) {
    busy.value = false
    error.value = e instanceof ApiError && e.status === 429
      ? `Too many attempts. Try again in ${e.retryAfter ?? 'a few'} seconds.`
      : (e as Error).message
  }
}
</script>

<template>
  <main class="grid min-h-screen place-items-center p-4">
    <Card class="w-full max-w-sm">
      <template #title>Sign in</template>
      <template #subtitle>Swagger Aggregator</template>
      <template #content>
        <form class="flex flex-col gap-4" @submit.prevent="submit">
          <div class="flex flex-col gap-2">
            <label for="username">Username</label>
            <InputText id="username" v-model="username" autocomplete="username" autofocus fluid />
          </div>
          <div class="flex flex-col gap-2">
            <label for="password">Password</label>
            <Password v-model="password" input-id="password" :feedback="false" toggle-mask fluid
              :input-props="{ autocomplete: 'current-password' }" />
          </div>
          <Message v-if="error" severity="error" size="small" variant="simple">{{ error }}</Message>
          <Button type="submit" label="Sign in" icon="pi pi-sign-in" :loading="busy" fluid />
        </form>
      </template>
    </Card>
  </main>
</template>
