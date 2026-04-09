<template>
  <div
    v-if="selectedIds.length > 0"
    class="mb-4 flex flex-wrap items-center justify-between gap-3 rounded-lg bg-primary-50 p-3 dark:bg-primary-900/20"
  >
    <div class="flex flex-wrap items-center gap-2">
      <span class="text-sm font-medium text-primary-900 dark:text-primary-100">
        {{ t('admin.users.bulkActions.selected', { count: selectedIds.length }) }}
      </span>
      <button
        @click="$emit('select-page')"
        class="text-xs font-medium text-primary-700 hover:text-primary-800 dark:text-primary-300 dark:hover:text-primary-200"
      >
        {{ t('admin.users.bulkActions.selectCurrentPage') }}
      </button>
      <span class="text-gray-300 dark:text-primary-800">•</span>
      <button
        @click="$emit('clear')"
        class="text-xs font-medium text-primary-700 hover:text-primary-800 dark:text-primary-300 dark:hover:text-primary-200"
      >
        {{ t('admin.users.bulkActions.clear') }}
      </button>
    </div>

    <div class="flex flex-wrap gap-2">
      <button @click="$emit('delete')" class="btn btn-danger btn-sm">
        {{ t('admin.users.bulkActions.delete') }}
      </button>
      <button @click="$emit('enable')" class="btn btn-success btn-sm">
        {{ t('admin.users.bulkActions.enable') }}
      </button>
      <button @click="$emit('disable')" class="btn btn-warning btn-sm">
        {{ t('admin.users.bulkActions.disable') }}
      </button>
      <button @click="$emit('deposit')" class="btn btn-primary btn-sm">
        {{ t('admin.users.bulkActions.deposit') }}
      </button>
      <button @click="$emit('withdraw')" class="btn btn-secondary btn-sm">
        {{ t('admin.users.bulkActions.withdraw') }}
      </button>
    </div>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'

defineProps<{
  selectedIds: number[]
}>()

defineEmits<{
  (e: 'delete'): void
  (e: 'enable'): void
  (e: 'disable'): void
  (e: 'deposit'): void
  (e: 'withdraw'): void
  (e: 'clear'): void
  (e: 'select-page'): void
}>()

const { t } = useI18n()
</script>
