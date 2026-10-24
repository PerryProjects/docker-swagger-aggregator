<script setup lang="ts">
import { computed, defineAsyncComponent } from 'vue'
import NavBar from '../components/NavBar.vue'
import { BASE } from '../base'

// The documentation of this program's own interface (what a custom frontend talks to): the REST endpoints, the
// live event stream (SSE) and the MCP endpoint, as one OpenAPI document rendered by Scalar.
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
  url: `${BASE}spec/openapi.json`,
}))
</script>

<template>
  <NavBar />
  <ApiReference :configuration="configuration" />
</template>
