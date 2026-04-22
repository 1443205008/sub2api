import { apiClient } from '../client'

export type BackupStorageType = 's3' | 'webdav'

export interface BackupStorageConfig {
  type: BackupStorageType
  prefix: string

  // S3-compatible
  endpoint: string
  region: string
  bucket: string
  access_key_id: string
  secret_access_key?: string
  force_path_style: boolean

  // WebDAV
  base_url: string
  username: string
  password?: string
}

export interface BackupScheduleConfig {
  enabled: boolean
  cron_expr: string
  retain_days: number
  retain_count: number
}

export interface BackupRecord {
  id: string
  status: 'pending' | 'running' | 'completed' | 'failed'
  backup_type: string
  file_name: string
  s3_key: string
  size_bytes: number
  triggered_by: string
  error_message?: string
  started_at: string
  finished_at?: string
  expires_at?: string
  progress?: string
  restore_status?: string
  restore_error?: string
  restored_at?: string
}

export interface CreateBackupRequest {
  expire_days?: number
}

export interface TestS3Response {
  ok: boolean
  message: string
}

// Storage Config
export async function getStorageConfig(): Promise<BackupStorageConfig> {
  const { data } = await apiClient.get<BackupStorageConfig>('/admin/backups/storage-config')
  return data
}

export async function updateStorageConfig(config: BackupStorageConfig): Promise<BackupStorageConfig> {
  const { data } = await apiClient.put<BackupStorageConfig>('/admin/backups/storage-config', config)
  return data
}

export async function testStorageConnection(config: BackupStorageConfig): Promise<TestS3Response> {
  const { data } = await apiClient.post<TestS3Response>('/admin/backups/storage-config/test', config)
  return data
}

// Schedule
export async function getSchedule(): Promise<BackupScheduleConfig> {
  const { data } = await apiClient.get<BackupScheduleConfig>('/admin/backups/schedule')
  return data
}

export async function updateSchedule(config: BackupScheduleConfig): Promise<BackupScheduleConfig> {
  const { data } = await apiClient.put<BackupScheduleConfig>('/admin/backups/schedule', config)
  return data
}

// Backup operations
export async function createBackup(req?: CreateBackupRequest): Promise<BackupRecord> {
  const { data } = await apiClient.post<BackupRecord>('/admin/backups', req || {})
  return data
}

export async function listBackups(): Promise<{ items: BackupRecord[] }> {
  const { data } = await apiClient.get<{ items: BackupRecord[] }>('/admin/backups')
  return data
}

export async function getBackup(id: string): Promise<BackupRecord> {
  const { data } = await apiClient.get<BackupRecord>(`/admin/backups/${id}`)
  return data
}

export async function deleteBackup(id: string): Promise<void> {
  await apiClient.delete(`/admin/backups/${id}`)
}

export async function getDownloadURL(id: string): Promise<{ url: string }> {
  const { data } = await apiClient.get<{ url: string }>(`/admin/backups/${id}/download-url`)
  return data
}

export async function downloadBackup(id: string): Promise<Blob> {
  const { data } = await apiClient.get<Blob>(`/admin/backups/${id}/download`, {
    responseType: 'blob',
  })
  return data
}

// Restore
export async function restoreBackup(id: string, password: string): Promise<BackupRecord> {
  const { data } = await apiClient.post<BackupRecord>(`/admin/backups/${id}/restore`, { password })
  return data
}

export const backupAPI = {
  getStorageConfig,
  updateStorageConfig,
  testStorageConnection,
  getSchedule,
  updateSchedule,
  createBackup,
  listBackups,
  getBackup,
  deleteBackup,
  getDownloadURL,
  downloadBackup,
  restoreBackup,
}

export default backupAPI
