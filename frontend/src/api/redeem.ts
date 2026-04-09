/**
 * Redeem code API endpoints
 * Handles redeem code redemption for users
 */

import { apiClient } from './client'
import type { RedeemCodeRequest } from '@/types'

export interface RedeemHistoryItem {
  id: number
  code: string
  type: string
  value: number
  status: string
  used_at: string
  created_at: string
  // Notes from admin for admin_balance/admin_concurrency types
  notes?: string
  // Subscription-specific fields
  group_id?: number
  validity_days?: number
  group?: {
    id: number
    name: string
  }
}

export interface InviteCodeItem {
  code: string
  status: string
  used_by?: number
  used_at?: string
  created_at: string
}

export interface InviteOverview {
  cashback_rate: number
  invited_users: number
  total_cashback: number
  codes: InviteCodeItem[]
}

/**
 * Redeem a code
 * @param code - Redeem code string
 * @returns Redemption result with updated balance or concurrency
 */
export async function redeem(code: string): Promise<{
  message: string
  type: string
  value: number
  new_balance?: number
  new_concurrency?: number
}> {
  const payload: RedeemCodeRequest = { code }

  const { data } = await apiClient.post<{
    message: string
    type: string
    value: number
    new_balance?: number
    new_concurrency?: number
  }>('/redeem', payload)

  return data
}

/**
 * Get user's redemption history
 * @returns List of redeemed codes
 */
export async function getHistory(): Promise<RedeemHistoryItem[]> {
  const { data } = await apiClient.get<RedeemHistoryItem[]>('/redeem/history')
  return data
}

/**
 * Generate an invitation code for current user
 */
export async function generateInviteCode(): Promise<InviteCodeItem> {
  const { data } = await apiClient.post<InviteCodeItem>('/redeem/invite-code')
  return data
}

/**
 * Get invite cashback overview for current user
 */
export async function getInviteOverview(limit = 10): Promise<InviteOverview> {
  const { data } = await apiClient.get<InviteOverview>('/redeem/invite-overview', {
    params: { limit }
  })
  return data
}

export const redeemAPI = {
  redeem,
  getHistory,
  generateInviteCode,
  getInviteOverview
}

export default redeemAPI
