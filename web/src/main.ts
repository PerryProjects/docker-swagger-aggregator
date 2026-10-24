import { createApp } from 'vue'
import App from './App.vue'
import { route } from './base'
import { useSession } from './composables/useSession'

// The session is known before the first render: it decides which pages exist (login, status, users) and
// whether PrimeVue is needed at all. The documentation view must not get PrimeVue's injected theme styles
// (nor Tailwind), so the plugin is installed - and downloaded - only for the pages that use it.
async function start() {
  const info = await useSession().load()
  const app = createApp(App)
  const needsShell = route === 'status' || route === 'users' || (info.mode === 'build_in' && !info.user)
  if (needsShell) {
    const [{ default: PrimeVue }, { default: ToastService }, { default: ConfirmationService }, { default: Aura }] =
      await Promise.all([
        import('primevue/config'),
        import('primevue/toastservice'),
        import('primevue/confirmationservice'),
        import('@primeuix/themes/aura'),
      ])
    app
      .use(PrimeVue, {
        // PrimeVue 5 verifies this key offline in the browser (a missing, invalid or expired key shows a banner).
        // It is not in the repository: it is given at build time as VITE_PRIMEVUE_LICENSE (see README, "UI stack and licences").
        license: import.meta.env.VITE_PRIMEVUE_LICENSE,
        theme: {
          preset: Aura,
          // follow the OS setting; the layer name matches the order declared in style.css
          options: { darkModeSelector: 'system', cssLayer: { name: 'primevue', order: 'theme, base, primevue' } },
        },
      })
      .use(ToastService)
      .use(ConfirmationService)
    // The row expander announces its state by default ("Row Expanded"); say what the button does instead.
    const aria = app.config.globalProperties.$primevue.config.locale?.aria
    if (aria) Object.assign(aria, { expandRow: 'Hide details', collapseRow: 'Show details' })
  }
  app.mount('#app')
}

void start()
