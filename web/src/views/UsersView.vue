<script setup lang="ts">
import { onMounted, ref } from 'vue'
import Button from 'primevue/button'
import Column from 'primevue/column'
import DataTable from 'primevue/datatable'
import Dialog from 'primevue/dialog'
import InputText from 'primevue/inputtext'
import Message from 'primevue/message'
import Password from 'primevue/password'
import Select from 'primevue/select'
import Tag from 'primevue/tag'
import ToggleSwitch from 'primevue/toggleswitch'
import { useConfirm } from 'primevue/useconfirm'
import { useToast } from 'primevue/usetoast'
import { createUser, deleteUser, listUsers, updateUser, type Role, type User } from '../api/auth'
import { useSession } from '../composables/useSession'

const toast = useToast()
const confirm = useConfirm()
const { session } = useSession()

const users = ref<User[]>([])
const loading = ref(true)
const loadError = ref('')

async function reload() {
  try {
    users.value = await listUsers()
    loadError.value = ''
  } catch (e) {
    loadError.value = (e as Error).message
  } finally {
    loading.value = false
  }
}
onMounted(reload)

const roles: { label: string; value: Role }[] = [
  { label: 'Admin (manages users)', value: 'admin' },
  { label: 'User (reads the docs)', value: 'user' },
]

// One dialog for both creating (editing === null) and editing a user.
const dialog = ref(false)
const editing = ref<User | null>(null)
const form = ref({ username: '', password: '', role: 'user' as Role, disabled: false })
const formError = ref('')
const saving = ref(false)

function openCreate() {
  editing.value = null
  form.value = { username: '', password: '', role: 'user', disabled: false }
  formError.value = ''
  dialog.value = true
}

function openEdit(u: User) {
  editing.value = u
  form.value = { username: u.username, password: '', role: u.role, disabled: u.disabled }
  formError.value = ''
  dialog.value = true
}

async function save() {
  formError.value = ''
  saving.value = true
  try {
    if (editing.value) {
      const u = editing.value
      const patch: { password?: string; role?: Role; disabled?: boolean } = {}
      if (form.value.role !== u.role) patch.role = form.value.role
      if (form.value.disabled !== u.disabled) patch.disabled = form.value.disabled
      if (form.value.password) patch.password = form.value.password
      if (Object.keys(patch).length) await updateUser(u.id, patch)
      toast.add({ severity: 'success', summary: 'User updated', detail: u.username, life: 3000 })
    } else {
      await createUser(form.value.username, form.value.password, form.value.role)
      toast.add({ severity: 'success', summary: 'User created', detail: form.value.username, life: 3000 })
    }
    dialog.value = false
    await reload()
  } catch (e) {
    formError.value = (e as Error).message
  } finally {
    saving.value = false
  }
}

function remove(u: User) {
  confirm.require({
    header: 'Delete user',
    message: `Delete “${u.username}”? Their sessions end immediately.`,
    icon: 'pi pi-exclamation-triangle',
    rejectProps: { label: 'Cancel', severity: 'secondary', text: true },
    acceptProps: { label: 'Delete', severity: 'danger' },
    accept: async () => {
      try {
        await deleteUser(u.id)
        toast.add({ severity: 'success', summary: 'User deleted', detail: u.username, life: 3000 })
        await reload()
      } catch (e) {
        toast.add({ severity: 'error', summary: 'Could not delete', detail: (e as Error).message, life: 6000 })
      }
    },
  })
}
</script>

<template>
  <main class="mx-auto max-w-4xl p-4 sm:p-6">
    <header class="mb-4 flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-2xl font-semibold">Users</h1>
      <Button label="New user" icon="pi pi-user-plus" @click="openCreate" />
    </header>

    <Message v-if="loadError" severity="error">{{ loadError }}</Message>
    <DataTable v-else :value="users" :loading="loading" data-key="id" size="small" striped-rows>
      <template #empty>No users.</template>
      <Column header="Username">
        <template #body="{ data }">
          <span class="font-medium" :data-user="data.username">{{ data.username }}</span>
          <span v-if="data.id === session?.user?.id" class="ml-2 text-sm text-muted-color">(you)</span>
        </template>
      </Column>
      <Column header="Role">
        <template #body="{ data }">
          <Tag :severity="data.role === 'admin' ? 'info' : 'secondary'" :value="data.role" />
        </template>
      </Column>
      <Column header="Status">
        <template #body="{ data }">
          <Tag :severity="data.disabled ? 'danger' : 'success'" :value="data.disabled ? 'disabled' : 'active'" />
        </template>
      </Column>
      <Column header="" class="text-right">
        <template #body="{ data }">
          <div class="flex justify-end gap-1">
            <Button icon="pi pi-pencil" severity="secondary" text rounded :aria-label="`Edit ${data.username}`" @click="openEdit(data)" />
            <Button
              icon="pi pi-trash"
              severity="danger"
              text
              rounded
              :aria-label="`Delete ${data.username}`"
              :disabled="data.id === session?.user?.id"
              @click="remove(data)"
            />
          </div>
        </template>
      </Column>
    </DataTable>

    <Dialog v-model:visible="dialog" modal :header="editing ? `Edit ${editing.username}` : 'New user'" class="w-full max-w-md">
      <form class="flex flex-col gap-4" @submit.prevent="save">
        <div v-if="!editing" class="flex flex-col gap-2">
          <label for="user-name">Username</label>
          <InputText id="user-name" v-model="form.username" autocomplete="off" autofocus fluid />
          <small class="text-muted-color">3–64 characters: a–z, 0–9, dot, underscore, dash.</small>
        </div>
        <div class="flex flex-col gap-2">
          <label for="user-password">{{ editing ? 'New password (leave empty to keep)' : 'Password (at least 10 characters)' }}</label>
          <Password v-model="form.password" input-id="user-password" toggle-mask fluid :input-props="{ autocomplete: 'new-password' }" />
        </div>
        <div class="flex flex-col gap-2">
          <label for="user-role">Role</label>
          <Select v-model="form.role" input-id="user-role" :options="roles" option-label="label" option-value="value" fluid />
        </div>
        <div v-if="editing" class="flex items-center gap-3">
          <ToggleSwitch v-model="form.disabled" input-id="user-disabled" />
          <label for="user-disabled">Disabled (cannot sign in)</label>
        </div>
        <Message v-if="formError" severity="error" size="small" variant="simple">{{ formError }}</Message>
        <div class="flex justify-end gap-2">
          <Button label="Cancel" severity="secondary" text @click="dialog = false" />
          <Button type="submit" :label="editing ? 'Save' : 'Create'" :loading="saving" />
        </div>
      </form>
    </Dialog>
  </main>
</template>
