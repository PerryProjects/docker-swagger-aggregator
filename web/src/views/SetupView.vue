<script setup lang="ts">
import { ref } from 'vue'
import Button from 'primevue/button'
import Card from 'primevue/card'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import Password from 'primevue/password'
import { setup } from '../api/auth'

const token = ref('')
const username = ref('admin')
const password = ref('')
const repeat = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  if (password.value !== repeat.value) {
    error.value = 'The passwords do not match.'
    return
  }
  busy.value = true
  try {
    await setup(token.value.trim(), username.value, password.value)
    location.reload()
  } catch (e) {
    busy.value = false
    error.value = (e as Error).message
  }
}
</script>

<template>
  <main class="grid min-h-screen place-items-center p-4">
    <Card class="w-full max-w-md">
      <template #title>Create the first admin</template>
      <template #subtitle>This instance has no users yet.</template>
      <template #content>
        <form class="flex flex-col gap-4" @submit.prevent="submit">
          <Message severity="info" size="small" variant="simple">
            The setup token is printed in the log of the web server at startup
            (<code>docker compose logs web</code>), or set with <code>SWAGGER_AGGREGATOR_WEB_SETUP_TOKEN</code>.
          </Message>
          <div class="flex flex-col gap-2">
            <label for="setup-token">Setup token</label>
            <InputText id="setup-token" v-model="token" autocomplete="off" autofocus fluid />
          </div>
          <div class="flex flex-col gap-2">
            <label for="setup-username">Username</label>
            <InputText id="setup-username" v-model="username" autocomplete="username" fluid />
          </div>
          <div class="flex flex-col gap-2">
            <label for="setup-password">Password (at least 10 characters)</label>
            <Password v-model="password" input-id="setup-password" toggle-mask fluid
              :input-props="{ autocomplete: 'new-password' }" />
          </div>
          <div class="flex flex-col gap-2">
            <label for="setup-repeat">Repeat password</label>
            <Password v-model="repeat" input-id="setup-repeat" :feedback="false" toggle-mask fluid
              :input-props="{ autocomplete: 'new-password' }" />
          </div>
          <Message v-if="error" severity="error" size="small" variant="simple">{{ error }}</Message>
          <Button type="submit" label="Create admin" icon="pi pi-user-plus" :loading="busy" fluid />
        </form>
      </template>
    </Card>
  </main>
</template>
