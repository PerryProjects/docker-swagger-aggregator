<script setup lang="ts">
import ReferenceView from './views/ReferenceView.vue'
import StatusView from './views/StatusView.vue'

// Path-based on purpose: Scalar owns the URL hash, so hash routing would clash.
const isStatus = location.pathname.replace(/\/+$/, '') === '/status'
</script>

<template>
  <StatusView v-if="isStatus" />
  <ReferenceView v-else />
</template>

<style>
html, body, #app { height: 100%; margin: 0; }
</style>
