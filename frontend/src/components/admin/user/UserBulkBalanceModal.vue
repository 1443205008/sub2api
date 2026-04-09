<template>
  <BaseDialog
    :show="show"
    :title="operation === 'add' ? t('admin.users.bulkBalance.depositTitle') : t('admin.users.bulkBalance.withdrawTitle')"
    width="narrow"
    @close="$emit('close')"
  >
    <form id="bulk-balance-form" class="space-y-5" @submit.prevent="handleSubmit">
      <div class="rounded-xl bg-gray-50 p-4 text-sm text-gray-700 dark:bg-dark-700 dark:text-gray-300">
        <div class="font-medium text-gray-900 dark:text-white">
          {{ t('admin.users.bulkBalance.selectedInfo', { count: selectedCount }) }}
        </div>
        <div class="mt-1 text-xs text-gray-500 dark:text-gray-400">
          {{ t('admin.users.bulkBalance.partialHint') }}
        </div>
      </div>

      <div>
        <label class="input-label">
          {{ operation === 'add' ? t('admin.users.depositAmount') : t('admin.users.withdrawAmount') }}
        </label>
        <div class="relative">
          <div class="absolute left-3 top-1/2 -translate-y-1/2 font-medium text-gray-500">$</div>
          <input
            v-model.number="form.amount"
            type="number"
            step="any"
            min="0"
            required
            class="input pl-8"
          />
        </div>
      </div>

      <div>
        <label class="input-label">{{ t('admin.users.notes') }}</label>
        <textarea v-model="form.notes" rows="3" class="input"></textarea>
      </div>
    </form>

    <template #footer>
      <div class="flex justify-end gap-3">
        <button @click="$emit('close')" class="btn btn-secondary">
          {{ t('common.cancel') }}
        </button>
        <button
          type="submit"
          form="bulk-balance-form"
          :disabled="submitting || !form.amount || form.amount <= 0"
          class="btn"
          :class="operation === 'add' ? 'bg-emerald-600 text-white' : 'btn-danger'"
        >
          {{
            submitting
              ? t('common.saving')
              : operation === 'add'
                ? t('admin.users.bulkBalance.confirmDeposit')
                : t('admin.users.bulkBalance.confirmWithdraw')
          }}
        </button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { reactive, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'

const props = defineProps<{
  show: boolean
  selectedCount: number
  operation: 'add' | 'subtract'
  submitting?: boolean
}>()

const emit = defineEmits<{
  (e: 'close'): void
  (e: 'submit', payload: { amount: number; notes: string }): void
}>()

const { t } = useI18n()

const form = reactive({
  amount: 0,
  notes: ''
})

watch(
  () => props.show,
  (visible) => {
    if (!visible) return
    form.amount = 0
    form.notes = ''
  }
)

const handleSubmit = () => {
  if (!form.amount || form.amount <= 0) return
  emit('submit', {
    amount: form.amount,
    notes: form.notes.trim()
  })
}
</script>
