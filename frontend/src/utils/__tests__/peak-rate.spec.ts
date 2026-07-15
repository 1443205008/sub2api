import { describe, expect, it } from 'vitest'
import { formatPeakRateWindow, hasPeakRate } from '../peak-rate'

describe('peak rate formatting', () => {
  it('formats multiple time rules with the server timezone', () => {
    const fields = {
      rate_time_rules: [
        { start: '08:00', end: '12:00', multiplier: 0.8 },
        { start: '22:00', end: '02:00', multiplier: 1.5 }
      ]
    }

    expect(hasPeakRate(fields)).toBe(true)
    expect(formatPeakRateWindow(fields, 'UTC+08:00')).toBe(
      '08:00-12:00 ×0.8; 22:00-02:00 ×1.5 (UTC+08:00)'
    )
  })

  it('keeps formatting legacy peak rate fields', () => {
    const fields = {
      peak_rate_enabled: true,
      peak_start: '14:00',
      peak_end: '18:00',
      peak_rate_multiplier: 2
    }

    expect(formatPeakRateWindow(fields)).toBe('14:00-18:00 ×2')
  })
})
