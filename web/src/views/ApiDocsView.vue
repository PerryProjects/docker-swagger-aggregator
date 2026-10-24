<script setup lang="ts">
import { computed, defineAsyncComponent } from 'vue'
import NavBar from '../components/NavBar.vue'
import { BASE } from '../base'

// The documentation of this program's own interface (what a custom frontend talks to): the REST endpoints as
// OpenAPI and the live event stream as AsyncAPI, both rendered by Scalar.
const ApiReference = defineAsyncComponent(async () => {
  const [{ ApiReference }] = await Promise.all([
    import('@scalar/api-reference'),
    import('@scalar/api-reference/style.css'),
  ])
  return ApiReference
})

const configuration = computed(() => ({
  withDefaultFonts: false,
  telemetry: false,
  agent: { disabled: true },
  hideClientButton: true,
  sources: [
    { title: 'REST API (OpenAPI)', slug: 'rest', url: `${BASE}spec/openapi.json` },
    { title: 'Events (AsyncAPI)', slug: 'events', url: `${BASE}spec/asyncapi.json` },
  ],
}))
</script>

<template>
  <NavBar />
  <ApiReference :configuration="configuration" />
</template>
