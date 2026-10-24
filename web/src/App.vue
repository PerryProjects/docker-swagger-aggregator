<script setup lang="ts">
import { computed, defineAsyncComponent } from 'vue'
import { BASE, route } from './base'
import { useSession } from './composables/useSession'

// Scalar (the documentation) and the PrimeVue/Tailwind pages are separate chunks: the docs never load
// Tailwind's global styles, and the login form never loads Scalar.
const ReferenceView = defineAsyncComponent(() => import('./views/ReferenceView.vue'))
const AppShell = defineAsyncComponent(() => import('./shell/AppShell.vue'))
const LoginView = defineAsyncComponent(() => import('./views/LoginView.vue'))
const SetupView = defineAsyncComponent(() => import('./views/SetupView.vue'))
const StatusView = defineAsyncComponent(() => import('./views/StatusView.vue'))
const UsersView = defineAsyncComponent(() => import('./views/UsersView.vue'))

const { session } = useSession() // loaded by main.ts before the app starts

// With built-in auth and nobody signed in, every route shows the login (or, on a new instance, the setup).
const needsLogin = computed(() => session.value?.mode === 'build_in' && !session.value.user)
const isAdmin = computed(() => session.value?.user?.role === 'admin')

// Users is for admins; anyone else is sent to the documentation. (The session is loaded before the app starts.)
if (route === 'users' && session.value && !needsLogin.value && !isAdmin.value) location.replace(BASE)
</script>

<template>
  <template v-if="session">
    <AppShell v-if="needsLogin" bare>
      <SetupView v-if="session.setupRequired" />
      <LoginView v-else />
    </AppShell>
    <AppShell v-else-if="route === 'status'"><StatusView /></AppShell>
    <AppShell v-else-if="route === 'users' && isAdmin"><UsersView /></AppShell>
    <ReferenceView v-else-if="route === 'docs'" />
  </template>
</template>

<style>
html, body, #app { height: 100%; margin: 0; }
</style>
