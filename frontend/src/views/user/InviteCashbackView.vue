<template>
  <AppLayout>
    <div class="invite-page-layout">
      <div class="card flex-1 min-h-0 overflow-hidden">
        <div v-if="loading" class="flex h-full items-center justify-center py-12">
          <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
        </div>

        <div v-else-if="!inviteEnabled" class="flex h-full items-center justify-center p-10 text-center">
          <div class="max-w-md">
            <div class="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-gray-100 dark:bg-dark-700">
              <Icon name="users" size="lg" class="text-gray-400" />
            </div>
            <h3 class="text-lg font-semibold text-gray-900 dark:text-white">
              {{ t('invitePage.notEnabledTitle') }}
            </h3>
            <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">
              {{ t('invitePage.notEnabledDesc') }}
            </p>
          </div>
        </div>

        <div v-else-if="hasConfiguredUrl && !isValidUrl" class="flex h-full items-center justify-center p-10 text-center">
          <div class="max-w-md">
            <div class="mx-auto mb-4 flex h-12 w-12 items-center justify-center rounded-full bg-gray-100 dark:bg-dark-700">
              <Icon name="link" size="lg" class="text-gray-400" />
            </div>
            <h3 class="text-lg font-semibold text-gray-900 dark:text-white">
              {{ t('invitePage.notConfiguredTitle') }}
            </h3>
            <p class="mt-2 text-sm text-gray-500 dark:text-dark-400">
              {{ t('invitePage.notConfiguredDesc') }}
            </p>
          </div>
        </div>

        <div v-else-if="hasConfiguredUrl" class="purchase-embed-shell">
          <a
            :href="inviteUrl"
            target="_blank"
            rel="noopener noreferrer"
            class="btn btn-secondary btn-sm purchase-open-fab"
          >
            <Icon name="externalLink" size="sm" class="mr-1.5" :stroke-width="2" />
            {{ t('invitePage.openInNewTab') }}
          </a>
          <iframe :src="inviteUrl" class="purchase-embed-frame" allowfullscreen></iframe>
        </div>

        <div v-else class="h-full overflow-y-auto p-6">
          <div class="mx-auto max-w-4xl space-y-6">
            <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <div>
                <h2 class="text-lg font-semibold text-gray-900 dark:text-white">{{ t('profile.invite.title') }}</h2>
                <p class="mt-1 text-sm text-gray-500 dark:text-dark-400">{{ t('profile.invite.description') }}</p>
              </div>
              <button class="btn btn-primary" :disabled="overviewLoading || generatingInviteCode" @click="handleGenerateInviteCode">
                <span v-if="generatingInviteCode">{{ t('profile.invite.generating') }}</span>
                <span v-else>{{ t('profile.invite.generateCode') }}</span>
              </button>
            </div>

            <div v-if="overviewLoading" class="flex items-center justify-center py-12">
              <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-500 border-t-transparent"></div>
            </div>

            <template v-else>
              <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
                <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-800">
                  <div class="text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.invitedUsers') }}</div>
                  <div class="mt-1 text-xl font-semibold text-gray-900 dark:text-white">{{ invitedUsers }}</div>
                </div>
                <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-800">
                  <div class="text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.cashbackRate') }}</div>
                  <div class="mt-1 text-xl font-semibold text-gray-900 dark:text-white">{{ cashbackRate }}</div>
                </div>
                <div class="rounded-xl bg-gray-50 p-4 dark:bg-dark-800">
                  <div class="text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.totalCashback') }}</div>
                  <div class="mt-1 text-xl font-semibold text-gray-900 dark:text-white">{{ formatCurrency(totalCashback) }}</div>
                </div>
              </div>

              <div class="rounded-2xl border border-gray-200 p-5 dark:border-dark-700">
                <div class="text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.latestCode') }}</div>
                <div class="mt-2 flex flex-col gap-2 sm:flex-row sm:items-center">
                  <code class="inline-flex min-h-[2.5rem] items-center rounded-lg bg-gray-100 px-3 py-2 font-mono text-sm text-gray-900 dark:bg-dark-800 dark:text-white">
                    {{ latestInviteCode || t('profile.invite.noCode') }}
                  </code>
                  <button class="btn btn-secondary" :disabled="!latestInviteCode" @click="copyInviteCode">
                    {{ t('profile.invite.copyCode') }}
                  </button>
                </div>
                <div class="mt-4 text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.inviteLink') }}</div>
                <div class="mt-2 flex flex-col gap-2 sm:flex-row sm:items-center">
                  <code class="inline-flex min-h-[2.5rem] flex-1 items-center overflow-x-auto rounded-lg bg-gray-100 px-3 py-2 text-sm text-gray-900 dark:bg-dark-800 dark:text-white">
                    {{ inviteLink || t('profile.invite.noLink') }}
                  </code>
                  <button class="btn btn-secondary" :disabled="!inviteLink" @click="copyInviteLink">
                    {{ t('profile.invite.copyLink') }}
                  </button>
                </div>
                <p class="mt-2 text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.help') }}</p>
              </div>

              <div class="rounded-2xl border border-gray-200 p-5 dark:border-dark-700">
                <div class="flex flex-col gap-4">
                  <div class="flex flex-col gap-3 lg:flex-row lg:items-end lg:justify-between">
                    <div>
                      <h3 class="text-sm font-semibold text-gray-900 dark:text-white">
                        {{ t('profile.invite.recordsTitle') }}
                      </h3>
                      <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
                        {{ t('profile.invite.recordsDescription') }}
                      </p>
                    </div>
                    <div class="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:min-w-[520px] lg:grid-cols-4">
                      <div>
                        <div class="mb-1 text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.recordStatus') }}</div>
                        <Select
                          v-model="recordFilters.status"
                          :options="recordStatusOptions"
                          @change="handleInviteRecordFilterChange"
                        />
                      </div>
                      <div>
                        <div class="mb-1 text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.recordSort') }}</div>
                        <Select
                          v-model="recordFilters.sort"
                          :options="recordSortOptions"
                          @change="handleInviteRecordFilterChange"
                        />
                      </div>
                      <div>
                        <div class="mb-1 text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.dateFrom') }}</div>
                        <input
                          v-model="recordFilters.date_from"
                          type="date"
                          class="input"
                          @change="handleInviteRecordFilterChange"
                        />
                      </div>
                      <div>
                        <div class="mb-1 text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.dateTo') }}</div>
                        <input
                          v-model="recordFilters.date_to"
                          type="date"
                          class="input"
                          @change="handleInviteRecordFilterChange"
                        />
                      </div>
                    </div>
                  </div>

                  <div>
                    <button class="btn btn-secondary btn-sm" @click="resetInviteRecordFilters">
                      {{ t('profile.invite.resetFilters') }}
                    </button>
                  </div>
                </div>

                <div v-if="inviteRecords.length === 0" class="mt-4 rounded-xl bg-gray-50 px-4 py-6 text-center text-sm text-gray-500 dark:bg-dark-800 dark:text-dark-400">
                  {{ t('profile.invite.noRecords') }}
                </div>

                <div v-else class="mt-4 space-y-3">
                  <div
                    v-for="record in inviteRecords"
                    :key="record.invited_user_id"
                    class="rounded-xl border border-gray-100 p-4 dark:border-dark-700"
                  >
                    <div class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
                      <div>
                        <div class="text-sm font-medium text-gray-900 dark:text-white">
                          {{ record.invited_user_email_masked || t('profile.invite.unknownUser') }}
                        </div>
                        <div class="mt-1 text-xs text-gray-500 dark:text-dark-400">
                          ID #{{ record.invited_user_id }}
                        </div>
                      </div>
                      <span
                        :class="[
                          'inline-flex w-fit rounded-full px-2.5 py-1 text-xs font-medium',
                          record.total_cashback > 0
                            ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
                            : 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-dark-300'
                        ]"
                      >
                        {{ record.total_cashback > 0 ? t('profile.invite.recharged') : t('profile.invite.notRecharged') }}
                      </span>
                    </div>

                    <div class="mt-4 grid grid-cols-1 gap-3 text-sm sm:grid-cols-3">
                      <div>
                        <div class="text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.registeredAt') }}</div>
                        <div class="mt-1 text-gray-900 dark:text-white">{{ formatInviteDate(record.registered_at || record.invite_used_at) }}</div>
                      </div>
                      <div>
                        <div class="text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.recordCashback') }}</div>
                        <div class="mt-1 text-gray-900 dark:text-white">{{ formatCurrency(record.total_cashback) }}</div>
                        <div class="mt-1 text-xs text-gray-500 dark:text-dark-400">
                          {{ t('profile.invite.cashbackCount', { count: record.cashback_count || 0 }) }}
                        </div>
                      </div>
                      <div>
                        <div class="text-xs text-gray-500 dark:text-dark-400">{{ t('profile.invite.lastCashbackAt') }}</div>
                        <div class="mt-1 text-gray-900 dark:text-white">
                          {{ record.last_cashback_at ? formatInviteDate(record.last_cashback_at) : t('profile.invite.noCashbackYet') }}
                        </div>
                      </div>
                    </div>
                  </div>
                </div>

                <Pagination
                  v-if="inviteRecordPagination.total > 0"
                  class="mt-4"
                  :page="inviteRecordPagination.page"
                  :total="inviteRecordPagination.total"
                  :page-size="inviteRecordPagination.page_size"
                  @update:page="handleInviteRecordPageChange"
                  @update:pageSize="handleInviteRecordPageSizeChange"
                />
              </div>
            </template>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores'
import { useAuthStore } from '@/stores/auth'
import { formatDateTime } from '@/utils/format'
import AppLayout from '@/components/layout/AppLayout.vue'
import Icon from '@/components/icons/Icon.vue'
import Pagination from '@/components/common/Pagination.vue'
import Select from '@/components/common/Select.vue'
import { buildEmbeddedUrl, detectTheme } from '@/utils/embedded-url'
import { redeemAPI } from '@/api'
import type { InviteOverview } from '@/api/redeem'

const { t, locale } = useI18n()
const appStore = useAppStore()
const authStore = useAuthStore()

const loading = ref(false)
const overviewLoading = ref(false)
const generatingInviteCode = ref(false)
const inviteTheme = ref<'light' | 'dark'>('light')
const inviteOverview = ref<InviteOverview | null>(null)
let themeObserver: MutationObserver | null = null
const recordFilters = ref({
  page: 1,
  page_size: 10,
  status: '',
  sort: 'registered_desc',
  date_from: '',
  date_to: ''
})

const inviteEnabled = computed(() => appStore.cachedPublicSettings?.invite_cashback_enabled ?? false)
const configuredBaseUrl = computed(() => (appStore.cachedPublicSettings?.invite_cashback_url || '').trim())
const hasConfiguredUrl = computed(() => configuredBaseUrl.value !== '')
const inviteUrl = computed(() => {
  return buildEmbeddedUrl(configuredBaseUrl.value, authStore.user?.id, authStore.token, inviteTheme.value, locale.value)
})
const isValidUrl = computed(() => {
  const url = inviteUrl.value
  return url.startsWith('http://') || url.startsWith('https://')
})

const latestInviteCode = computed(() => inviteOverview.value?.codes?.[0]?.code || '')
const inviteLink = computed(() => {
  if (!latestInviteCode.value || typeof window === 'undefined') return ''
  const url = new URL('/register', window.location.origin)
  url.searchParams.set('invitation_code', latestInviteCode.value)
  return url.toString()
})
const invitedUsers = computed(() => inviteOverview.value?.invited_users || 0)
const totalCashback = computed(() => inviteOverview.value?.total_cashback || 0)
const inviteRecords = computed(() => inviteOverview.value?.records?.items || [])
const inviteRecordPagination = computed(() => inviteOverview.value?.records || {
  items: [],
  total: 0,
  page: 1,
  page_size: recordFilters.value.page_size,
  pages: 1,
  sort_by: 'registered_at',
  sort_order: 'desc',
  status: ''
})
const cashbackRate = computed(() => {
  const rate = inviteOverview.value?.cashback_rate ?? appStore.cachedPublicSettings?.invite_cashback_rate ?? 0
  return `${Number(rate).toFixed(rate % 1 === 0 ? 0 : 2)}%`
})

const recordStatusOptions = computed(() => [
  { value: '', label: t('profile.invite.recordStatusAll') },
  { value: 'recharged', label: t('profile.invite.recharged') },
  { value: 'not_recharged', label: t('profile.invite.notRecharged') }
])

const recordSortOptions = computed(() => [
  { value: 'registered_desc', label: t('profile.invite.sortRegisteredDesc') },
  { value: 'registered_asc', label: t('profile.invite.sortRegisteredAsc') },
  { value: 'cashback_desc', label: t('profile.invite.sortCashbackDesc') },
  { value: 'cashback_asc', label: t('profile.invite.sortCashbackAsc') },
  { value: 'amount_desc', label: t('profile.invite.sortAmountDesc') },
  { value: 'amount_asc', label: t('profile.invite.sortAmountAsc') }
])

const shouldUseInternalPage = computed(() => inviteEnabled.value && !hasConfiguredUrl.value)

const loadInviteOverview = async () => {
  if (!shouldUseInternalPage.value) return
  overviewLoading.value = true
  try {
    const [sort_by, sort_order] = recordFilters.value.sort.split('_')
    inviteOverview.value = await redeemAPI.getInviteOverview({
      limit: 10,
      records_page: recordFilters.value.page,
      records_page_size: recordFilters.value.page_size,
      sort_by: sort_by === 'cashback' ? 'last_cashback_at' : sort_by === 'amount' ? 'total_cashback' : 'registered_at',
      sort_order: sort_order as 'asc' | 'desc',
      status: (recordFilters.value.status || '') as '' | 'recharged' | 'not_recharged',
      date_from: recordFilters.value.date_from || undefined,
      date_to: recordFilters.value.date_to || undefined
    })
  } catch (error) {
    console.error('Failed to load invite overview:', error)
  } finally {
    overviewLoading.value = false
  }
}

const handleGenerateInviteCode = async () => {
  generatingInviteCode.value = true
  try {
    await redeemAPI.generateInviteCode()
    await loadInviteOverview()
    appStore.showSuccess(t('profile.invite.generateSuccess'))
  } catch (error) {
    console.error('Failed to generate invite code:', error)
    appStore.showError(t('profile.invite.generateFailed'))
  } finally {
    generatingInviteCode.value = false
  }
}

const copyInviteCode = async () => {
  if (!latestInviteCode.value) return
  try {
    await navigator.clipboard.writeText(latestInviteCode.value)
    appStore.showSuccess(t('profile.invite.copySuccess'))
  } catch (error) {
    console.error('Failed to copy invite code:', error)
    appStore.showError(t('profile.invite.copyFailed'))
  }
}

const copyInviteLink = async () => {
  if (!inviteLink.value) return
  try {
    await navigator.clipboard.writeText(inviteLink.value)
    appStore.showSuccess(t('profile.invite.copyLinkSuccess'))
  } catch (error) {
    console.error('Failed to copy invite link:', error)
    appStore.showError(t('profile.invite.copyLinkFailed'))
  }
}

const formatInviteDate = (value?: string) => {
  if (!value) return '-'
  return formatDateTime(value)
}

const handleInviteRecordFilterChange = async () => {
  recordFilters.value.page = 1
  await loadInviteOverview()
}

const handleInviteRecordPageChange = async (page: number) => {
  recordFilters.value.page = page
  await loadInviteOverview()
}

const handleInviteRecordPageSizeChange = async (pageSize: number) => {
  recordFilters.value.page_size = pageSize
  recordFilters.value.page = 1
  await loadInviteOverview()
}

const resetInviteRecordFilters = async () => {
  recordFilters.value = {
    page: 1,
    page_size: 10,
    status: '',
    sort: 'registered_desc',
    date_from: '',
    date_to: ''
  }
  await loadInviteOverview()
}

watch(shouldUseInternalPage, async (enabled) => {
  if (enabled) {
    await loadInviteOverview()
  }
}, { immediate: false })

onMounted(async () => {
  inviteTheme.value = detectTheme()

  if (typeof document !== 'undefined') {
    themeObserver = new MutationObserver(() => {
      inviteTheme.value = detectTheme()
    })
    themeObserver.observe(document.documentElement, {
      attributes: true,
      attributeFilter: ['class'],
    })
  }

  if (!appStore.publicSettingsLoaded) {
    loading.value = true
    try {
      await appStore.fetchPublicSettings()
    } finally {
      loading.value = false
    }
  }

  await loadInviteOverview()
})

onUnmounted(() => {
  if (themeObserver) {
    themeObserver.disconnect()
    themeObserver = null
  }
})

const formatCurrency = (v: number) => `$${v.toFixed(2)}`
</script>

<style scoped>
.invite-page-layout {
  @apply flex flex-col;
  height: calc(100vh - 64px - 4rem);
}

.purchase-embed-shell {
  @apply relative;
  @apply h-full w-full overflow-hidden rounded-2xl;
  @apply bg-gradient-to-b from-gray-50 to-white dark:from-dark-900 dark:to-dark-950;
  @apply p-0;
}

.purchase-open-fab {
  @apply absolute right-3 top-3 z-10;
  @apply shadow-sm backdrop-blur supports-[backdrop-filter]:bg-white/80 dark:supports-[backdrop-filter]:bg-dark-800/80;
}

.purchase-embed-frame {
  display: block;
  margin: 0;
  width: 100%;
  height: 100%;
  border: 0;
  border-radius: 0;
  box-shadow: none;
  background: transparent;
}
</style>
