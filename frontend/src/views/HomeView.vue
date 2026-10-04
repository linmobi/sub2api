<template>
  <!-- Custom Home Content: Full Page Mode -->
  <div v-if="hasHomeContent" class="min-h-screen">
    <!-- iframe mode -->
    <iframe
      v-if="isHomeContentUrl"
      :src="homeContent.trim()"
      class="h-screen w-full border-0"
      allowfullscreen
    ></iframe>
    <!-- HTML mode - SECURITY: homeContent is admin-only setting, XSS risk is acceptable -->
    <div v-else v-html="homeContent"></div>
  </div>

  <!-- Compact Home Page -->
  <div
    v-else-if="compactHomeEnabled"
    data-testid="compact-home"
    class="flex min-h-screen flex-col bg-gray-50 text-gray-900 dark:bg-dark-950 dark:text-white"
  >
    <header class="border-b border-gray-200 px-4 py-4 sm:px-6 dark:border-dark-800">
      <nav class="mx-auto flex max-w-5xl flex-wrap items-center justify-between gap-3 sm:gap-4">
        <div class="flex min-w-0 flex-1 items-center gap-3">
          <img
            :src="siteLogo || '/logo.svg'"
            alt="Logo"
            class="h-9 w-9 shrink-0 rounded-lg object-contain"
          />
          <span class="min-w-0 truncate text-base font-semibold">{{ siteName }}</span>
        </div>
        <div class="flex max-w-full shrink-0 flex-wrap items-center justify-end gap-2">
          <LocaleSwitcher />
          <a
            v-if="docUrl"
            :href="docUrl"
            target="_blank"
            rel="noopener noreferrer"
            class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg text-gray-500 hover:bg-gray-100 dark:text-dark-400 dark:hover:bg-dark-800"
            :title="t('home.viewDocs')"
          >
            <Icon name="book" size="md" />
          </a>
          <router-link
            v-if="showModelPlazaEntry"
            to="/model-plaza"
            class="flex h-10 shrink-0 items-center gap-1.5 rounded-lg px-2.5 text-sm font-medium text-gray-500 hover:bg-gray-100 hover:text-gray-700 dark:text-dark-400 dark:hover:bg-dark-800 dark:hover:text-white"
            :title="t('nav.modelPlaza')"
          >
            <Icon name="grid" size="md" />
            <span class="hidden sm:inline">{{ t('nav.modelPlaza') }}</span>
          </router-link>
          <button
            class="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg text-gray-500 hover:bg-gray-100 dark:text-dark-400 dark:hover:bg-dark-800"
            :title="isDark ? t('home.switchToLight') : t('home.switchToDark')"
            @click="toggleTheme"
          >
            <Icon v-if="isDark" name="sun" size="md" />
            <Icon v-else name="moon" size="md" />
          </button>
          <router-link
            :to="isAuthenticated ? dashboardPath : '/login'"
            class="inline-flex min-h-10 shrink-0 items-center justify-center rounded-lg bg-gray-900 px-4 py-2 text-sm font-medium text-white hover:bg-gray-800 dark:bg-white dark:text-gray-900 dark:hover:bg-gray-200"
          >
            {{ isAuthenticated ? t('home.dashboard') : t('home.login') }}
          </router-link>
        </div>
      </nav>
    </header>

    <main class="flex min-w-0 flex-1 items-center justify-center px-4 py-16 sm:px-6">
      <div class="min-w-0 max-w-2xl text-center">
        <img
          :src="siteLogo || '/logo.svg'"
          alt="Logo"
          class="mx-auto mb-6 h-20 w-20 rounded-2xl object-contain"
        />
        <h1 class="[overflow-wrap:anywhere] text-3xl font-bold md:text-4xl">{{ siteName }}</h1>
        <p class="mt-4 whitespace-pre-wrap [overflow-wrap:anywhere] text-base text-gray-600 dark:text-dark-300">{{ siteSubtitle }}</p>
        <router-link
          :to="isAuthenticated ? dashboardPath : '/login'"
          class="mt-8 inline-flex min-h-10 items-center justify-center rounded-lg bg-primary-600 px-5 py-2.5 text-sm font-medium text-white hover:bg-primary-700"
        >
          {{ isAuthenticated ? t('home.goToDashboard') : t('home.login') }}
        </router-link>
      </div>
    </main>

    <footer class="min-w-0 border-t border-gray-200 px-4 py-5 text-center text-sm text-gray-500 [overflow-wrap:anywhere] sm:px-6 dark:border-dark-800 dark:text-dark-400">
      &copy; {{ currentYear }} {{ siteName }}
    </footer>
  </div>

  <!-- Clean default home; configured custom and compact pages remain available. -->
  <div v-else class="clean-home flex min-h-screen flex-col bg-white text-gray-900 dark:bg-dark-950 dark:text-white">
    <header class="border-b border-gray-100 px-5 dark:border-dark-800">
      <nav class="mx-auto flex min-h-20 max-w-6xl flex-wrap items-center justify-between gap-3 py-4">
        <router-link to="/home" class="flex min-w-0 items-center gap-2.5 font-semibold">
          <img :src="siteLogo || '/logo.svg'" alt="Logo" class="h-8 w-8 shrink-0 rounded-lg object-contain" />
          <span class="truncate">{{ siteName }}</span>
        </router-link>
        <div class="flex items-center gap-2">
          <a v-if="docUrl" :href="docUrl" target="_blank" rel="noopener noreferrer" class="btn btn-ghost hidden sm:inline-flex">{{ t('home.docs') }}</a>
          <router-link v-if="showModelPlazaEntry" to="/model-plaza" class="btn btn-ghost hidden sm:inline-flex">{{ t('nav.modelPlaza') }}</router-link>
          <LocaleSwitcher />
          <button type="button" class="btn btn-ghost btn-icon" :aria-label="isDark ? t('home.switchToLight') : t('home.switchToDark')" @click="toggleTheme">
            <Icon :name="isDark ? 'sun' : 'moon'" size="md" />
          </button>
          <router-link :to="isAuthenticated ? dashboardPath : '/login'" class="btn btn-secondary">{{ isAuthenticated ? t('home.dashboard') : t('home.login') }}</router-link>
        </div>
      </nav>
    </header>
    <main class="flex-1">
      <section class="mx-auto max-w-4xl px-5 pb-14 pt-16 text-center sm:pb-16 sm:pt-24">
        <div class="mb-6 inline-flex items-center gap-2 rounded-full bg-primary-50 px-3.5 py-1.5 text-xs font-medium text-primary-800 dark:bg-primary-900/30 dark:text-primary-200">
          <Icon name="server" size="sm" /> {{ t('home.tags.subscriptionToApi') }}
        </div>
        <h1 class="break-words text-4xl font-semibold tracking-tight sm:text-6xl">{{ siteName }}</h1>
        <p class="mx-auto mt-5 max-w-xl whitespace-pre-wrap break-words text-base leading-7 text-gray-500 sm:text-lg dark:text-dark-300">{{ siteSubtitle }}</p>
        <div class="mt-8 flex flex-wrap justify-center gap-3">
          <router-link :to="isAuthenticated ? dashboardPath : '/login'" class="btn btn-primary min-h-11 px-6">
            {{ isAuthenticated ? t('home.goToDashboard') : t('home.getStarted') }} <Icon name="arrowRight" size="sm" />
          </router-link>
          <router-link v-if="showModelPlazaEntry" to="/model-plaza" class="btn btn-secondary min-h-11 px-6">{{ t('nav.modelPlaza') }}</router-link>
          <a v-else-if="docUrl" :href="docUrl" target="_blank" rel="noopener noreferrer" class="btn btn-secondary min-h-11 px-6">{{ t('home.viewDocs') }}</a>
        </div>
        <div class="mt-7 flex flex-wrap justify-center gap-x-6 gap-y-2 text-xs text-gray-500 dark:text-dark-400">
          <span class="inline-flex items-center gap-1.5"><Icon name="shield" size="sm" />{{ t('home.tags.stickySession') }}</span>
          <span class="inline-flex items-center gap-1.5"><Icon name="chart" size="sm" />{{ t('home.tags.realtimeBilling') }}</span>
        </div>
      </section>
      <section class="border-y border-gray-100 bg-gray-50/60 px-5 py-7 dark:border-dark-800 dark:bg-dark-900/50">
        <div class="mx-auto flex max-w-5xl flex-col items-center justify-between gap-5 sm:flex-row">
          <div class="text-center sm:text-left">
            <h2 class="text-sm font-medium text-gray-700 dark:text-dark-200">{{ t('home.providers.title') }}</h2>
            <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('home.providers.description') }}</p>
          </div>
          <div class="flex flex-wrap items-center justify-center gap-x-7 gap-y-3 text-base font-semibold tracking-tight text-gray-600 dark:text-dark-300">
            <span>Claude</span><span>GPT</span><span>Gemini</span><span>Antigravity</span>
          </div>
        </div>
      </section>
      <section class="mx-auto grid max-w-6xl gap-5 px-5 py-12 sm:py-16 md:grid-cols-3">
        <article v-for="feature in homeFeatures" :key="feature.title" class="rounded-xl border border-gray-200 p-6 dark:border-dark-800">
          <div class="mb-5 inline-flex h-10 w-10 items-center justify-center rounded-lg bg-gray-50 text-primary-700 dark:bg-dark-900 dark:text-primary-300"><Icon :name="feature.icon" size="md" /></div>
          <h3 class="text-base font-semibold">{{ t(feature.title) }}</h3>
          <p class="mt-3 text-sm leading-7 text-gray-500 dark:text-dark-400">{{ t(feature.description) }}</p>
        </article>
      </section>
    </main>
    <footer class="border-t border-gray-100 px-5 py-6 dark:border-dark-800">
      <div class="mx-auto flex max-w-6xl flex-col items-center justify-between gap-3 text-xs text-gray-500 sm:flex-row dark:text-dark-400">
        <p>&copy; {{ currentYear }} {{ siteName }}. {{ t('home.footer.allRightsReserved') }}</p>
        <div class="flex gap-5"><a v-if="docUrl" :href="docUrl" target="_blank" rel="noopener noreferrer" class="hover:text-primary-700">{{ t('home.docs') }}</a><a :href="githubUrl" target="_blank" rel="noopener noreferrer" class="hover:text-primary-700">GitHub</a></div>
      </div>
    </footer>
  </div>
</template>

<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAuthStore, useAppStore } from '@/stores'
import LocaleSwitcher from '@/components/common/LocaleSwitcher.vue'
import Icon from '@/components/icons/Icon.vue'
import { sanitizeUrl } from '@/utils/url'
import { FeatureFlags, isFeatureFlagEnabled } from '@/utils/featureFlags'

const { t } = useI18n()
const homeFeatures = [
  { icon: 'server', title: 'home.features.unifiedGateway', description: 'home.features.unifiedGatewayDesc' },
  { icon: 'users', title: 'home.features.multiAccount', description: 'home.features.multiAccountDesc' },
  { icon: 'chart', title: 'home.features.balanceQuota', description: 'home.features.balanceQuotaDesc' }
] as const

const authStore = useAuthStore()
const appStore = useAppStore()

// Site settings - directly from appStore (already initialized from injected config)
const siteName = computed(() => appStore.cachedPublicSettings?.site_name || appStore.siteName || 'Sub2API')
const siteLogo = computed(() => sanitizeUrl(appStore.cachedPublicSettings?.site_logo || appStore.siteLogo || '', { allowRelative: true, allowDataUrl: true }))
const siteSubtitle = computed(() => appStore.cachedPublicSettings?.site_subtitle || 'AI API Gateway Platform')
const docUrl = computed(() => sanitizeUrl(appStore.cachedPublicSettings?.doc_url || appStore.docUrl || ''))
const homeContent = computed(() => appStore.cachedPublicSettings?.home_content || '')
const hasHomeContent = computed(() => homeContent.value.trim().length > 0)
const compactHomeEnabled = computed(() => appStore.cachedPublicSettings?.compact_home_enabled === true)
const modelPlazaEnabled = computed(() => isFeatureFlagEnabled(FeatureFlags.modelPlaza))

// Check if homeContent is a URL (for iframe display)
const isHomeContentUrl = computed(() => {
  const content = homeContent.value.trim()
  return content.startsWith('http://') || content.startsWith('https://')
})

// Theme
const isDark = ref(document.documentElement.classList.contains('dark'))

// GitHub URL
const githubUrl = 'https://github.com/Wei-Shaw/sub2api'

// Auth state
const isAuthenticated = computed(() => authStore.isAuthenticated)
const modelPlazaRequiresAuth = computed(
  () => appStore.cachedPublicSettings?.model_plaza_require_auth === true,
)
const showModelPlazaEntry = computed(
  () => modelPlazaEnabled.value && (isAuthenticated.value || !modelPlazaRequiresAuth.value),
)
const isAdmin = computed(() => authStore.isAdmin)
const dashboardPath = computed(() => isAdmin.value ? '/admin/dashboard' : '/dashboard')
// Current year for footer
const currentYear = computed(() => new Date().getFullYear())

// Toggle theme
function toggleTheme() {
  isDark.value = !isDark.value
  document.documentElement.classList.toggle('dark', isDark.value)
  localStorage.setItem('theme', isDark.value ? 'dark' : 'light')
}

// Initialize theme
function initTheme() {
  const savedTheme = localStorage.getItem('theme')
  if (
    savedTheme === 'dark' ||
    (!savedTheme && window.matchMedia('(prefers-color-scheme: dark)').matches)
  ) {
    isDark.value = true
    document.documentElement.classList.add('dark')
  }
}

onMounted(() => {
  initTheme()

  // Check auth state
  authStore.checkAuth()

  // Ensure public settings are loaded (will use cache if already loaded from injected config)
  if (!appStore.publicSettingsLoaded) {
    appStore.fetchPublicSettings()
  }
})
</script>
