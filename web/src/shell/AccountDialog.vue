<script setup lang="ts">
import { ref } from 'vue'
import Button from 'primevue/button'
import Dialog from 'primevue/dialog'
import Message from 'primevue/message'
import Password from 'primevue/password'
import { useToast } from 'primevue/usetoast'
import { changePassword } from '../api/auth'

const visible = defineModel<boolean>('visible', { required: true })
const toast = useToast()
const current = ref('')
const next = ref('')
const repeat = ref('')
const error = ref('')
const busy = ref(false)

async function submit() {
  error.value = ''
  if (next.value !== repeat.value) {
    error.value = 'The new passwords do not match.'
    return
  }
  busy.value = true
  try {
    await changePassword(current.value, next.value)
    toast.add({ severity: 'success', summary: 'Password changed', detail: 'Your other sessions were signed out.', life: 4000 })
    visible.value = false
    current.value = next.value = repeat.value = ''
  } catch (e) {
    error.value = (e as Error).message
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog v-model:visible="visible" modal header="Change password" class="w-full max-w-sm" @hide="error = ''">
    <form class="flex flex-col gap-4" @submit.prevent="submit">
      <div class="flex flex-col gap-2">
        <label for="current-password">Current password</label>
        <Password v-model="current" input-id="current-password" :feedback="false" toggle-mask fluid
          :input-props="{ autocomplete: 'current-password' }" />
      </div>
      <div class="flex flex-col gap-2">
        <label for="new-password">New password (at least 10 characters)</label>
        <Password v-model="next" input-id="new-password" toggle-mask fluid
          :input-props="{ autocomplete: 'new-password' }" />
      </div>
      <div class="flex flex-col gap-2">
        <label for="repeat-password">Repeat new password</label>
        <Password v-model="repeat" input-id="repeat-password" :feedback="false" toggle-mask fluid
          :input-props="{ autocomplete: 'new-password' }" />
      </div>
      <Message v-if="error" severity="error" size="small" variant="simple">{{ error }}</Message>
      <div class="flex justify-end gap-2">
        <Button label="Cancel" severity="secondary" text @click="visible = false" />
        <Button type="submit" label="Change password" :loading="busy" />
      </div>
    </form>
  </Dialog>
</template>
