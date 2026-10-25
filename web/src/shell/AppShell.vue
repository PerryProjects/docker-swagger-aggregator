<script setup lang="ts">
// The frame of every PrimeVue page (status, users, login, setup). It is loaded lazily and is the only place
// that imports the Tailwind CSS, so the Scalar documentation view stays untouched.
import '../style.css'
import { computed, ref } from 'vue'
import Button from 'primevue/button'
import ConfirmDialog from 'primevue/confirmdialog'
import Menubar from 'primevue/menubar'
import Menu from 'primevue/menu'
import Toast from 'primevue/toast'
import { BASE, route } from '../base'
import { useSession } from '../composables/useSession'
import AccountDialog from './AccountDialog.vue'

defineProps<{ bare?: boolean }>() // bare: no navigation (login, setup)

const { session, logout } = useSession()
const user = computed(() => session.value?.user)
const builtIn = computed(() => session.value?.mode === 'build_in')

const items = computed(() => [
  { label: 'Documentation', icon: 'pi pi-book', url: BASE, class: route === 'docs' ? 'font-semibold' : '' },
  { label: 'API', icon: 'pi pi-code', url: `${BASE}api-docs#rest`, class: route === 'api' ? 'font-semibold' : '' },
  { label: 'Events', icon: 'pi pi-bolt', url: `${BASE}api-docs#events` },
  { label: 'Status', icon: 'pi pi-heart', url: `${BASE}status`, class: route === 'status' ? 'font-semibold' : '' },
  ...(user.value?.role === 'admin'
    ? [{ label: 'Users', icon: 'pi pi-users', url: `${BASE}users`, class: route === 'users' ? 'font-semibold' : '' }]
    : []),
])

const accountOpen = ref(false)
const userMenu = ref<InstanceType<typeof Menu>>()
const userItems = [
  { label: 'Change password', icon: 'pi pi-key', command: () => (accountOpen.value = true) },
  { label: 'Sign out', icon: 'pi pi-sign-out', command: () => logout() },
]
</script>

<template>
  <div class="min-h-screen bg-surface-50 text-surface-900 dark:bg-surface-950 dark:text-surface-0">
    <Toast />
    <ConfirmDialog />
    <Menubar v-if="!bare" :model="items" class="rounded-none border-x-0 border-t-0">
      <template #start>
        <span class="mr-4 font-semibold">Swagger Aggregator</span>
      </template>
      <template #end>
        <div v-if="builtIn && user" class="flex items-center gap-2">
          <span class="text-sm text-muted-color" data-testid="current-user">{{ user.username }}</span>
          <Button
            icon="pi pi-user"
            severity="secondary"
            text
            rounded
            aria-label="Account menu"
            aria-haspopup="true"
            @click="(e: Event) => userMenu?.toggle(e)"
          />
          <Menu ref="userMenu" :model="userItems" popup />
        </div>
      </template>
    </Menubar>
    <slot />
    <AccountDialog v-model:visible="accountOpen" />
  </div>
</template>
