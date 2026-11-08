/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import type { PoolConfig, PoolMemberAnnotation, PoolStrategy } from '../types'

/**
 * One pool entry of the pool-scheduling dialog form. member_annotations is
 * kept as raw JSON text so the JSON editor round-trips unknown annotation
 * fields without narrowing them.
 */
export interface PoolFormPool {
  tag: string
  strategy: PoolStrategy
  rate_limit_policy: PoolConfig['rate_limit_policy']
  rate_limit_rest_seconds: number
  member_annotations_json: string
}

/**
 * Parse the pool_setting.pools option value (a JSON array string). Invalid
 * payloads degrade to an empty list so the dialog can still be opened and
 * repaired instead of crashing on malformed stored options.
 */
export function parsePoolsOptionValue(value: string | undefined): PoolConfig[] {
  if (!value || !value.trim()) {
    return []
  }
  try {
    const parsed: unknown = JSON.parse(value)
    if (!Array.isArray(parsed)) {
      return []
    }
    return parsed.filter(
      (pool): pool is PoolConfig =>
        typeof pool === 'object' &&
        pool !== null &&
        !Array.isArray(pool) &&
        typeof (pool as PoolConfig).tag === 'string'
    )
  } catch {
    return []
  }
}

/**
 * Convert a stored pool config into the dialog form entry, keeping the
 * member annotations as pretty-printed JSON text for the editor.
 */
export function toPoolFormValue(pool: PoolConfig): PoolFormPool {
  return {
    tag: pool.tag,
    strategy: pool.strategy,
    rate_limit_policy: pool.rate_limit_policy,
    rate_limit_rest_seconds: pool.rate_limit_rest_seconds,
    member_annotations_json: pool.member_annotations
      ? JSON.stringify(pool.member_annotations, null, 2)
      : '',
  }
}

/**
 * Convert form entries back into the pool_setting.pools option payload (a
 * JSON array string). member_annotations_json must already be validated JSON
 * (the zod schema enforces it before submit).
 */
export function buildPoolsOptionValue(pools: PoolFormPool[]): string {
  const configs: PoolConfig[] = pools.map((pool) => {
    let memberAnnotations: Record<string, PoolMemberAnnotation> | undefined
    const trimmed = pool.member_annotations_json.trim()
    if (trimmed) {
      memberAnnotations = JSON.parse(trimmed) as Record<
        string,
        PoolMemberAnnotation
      >
    }
    return {
      tag: pool.tag.trim(),
      strategy: pool.strategy,
      rate_limit_policy: pool.rate_limit_policy,
      rate_limit_rest_seconds: Math.max(
        0,
        Math.floor(pool.rate_limit_rest_seconds || 0)
      ),
      ...(memberAnnotations ? { member_annotations: memberAnnotations } : {}),
    }
  })
  return JSON.stringify(configs)
}
