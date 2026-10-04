<template>
  <div class="clean-auth min-h-screen bg-gray-50 dark:bg-dark-950">
    <header class="mx-auto flex max-w-6xl items-center px-6 py-6">
      <router-link to="/home" class="inline-flex items-center gap-2.5 rounded-lg text-sm font-semibold text-gray-900 dark:text-white">
        <img v-if="settingsLoaded" :src="siteLogo || '/logo.svg'" alt="Logo" class="h-8 w-8 rounded-lg object-contain" />
        {{ siteName }}
      </router-link>
    </header>
    <main class="mx-auto w-full max-w-md px-5 pb-12 pt-8 sm:pt-16">
      <div class="mb-7 text-center">
        <p class="text-sm leading-relaxed text-gray-500 dark:text-dark-400">{{ siteSubtitle }}</p>
      </div>
      <div class="clean-auth-card rounded-2xl border border-gray-200 bg-white p-6 sm:p-8 dark:border-dark-700 dark:bg-dark-900">
        <slot />
      </div>
      <div class="mt-6 text-center text-sm"><slot name="footer" /></div>
      <p class="mt-10 text-center text-xs text-gray-400 dark:text-dark-400">
        &copy; {{ currentYear }} {{ siteName }}. All rights reserved.
      </p>
    </main>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useAppStore } from '@/stores'
import { sanitizeUrl } from '@/utils/url'

const appStore = useAppStore()

const siteName = computed(() => appStore.siteName || 'Sub2API')
const siteLogo = computed(() => sanitizeUrl(appStore.siteLogo || '', { allowRelative: true, allowDataUrl: true }))
const siteSubtitle = computed(() => appStore.cachedPublicSettings?.site_subtitle || 'Subscription to API Conversion Platform')
const settingsLoaded = computed(() => appStore.publicSettingsLoaded)

const currentYear = computed(() => new Date().getFullYear())

onMounted(() => {
  appStore.fetchPublicSettings()
})
</script>
