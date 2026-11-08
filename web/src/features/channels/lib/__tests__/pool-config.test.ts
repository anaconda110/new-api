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
import { describe, expect, test } from 'vitest'

import {
  buildPoolsOptionValue,
  parsePoolsOptionValue,
  toPoolFormValue,
} from '../pool-config'

describe('parsePoolsOptionValue', () => {
  test('parses a stored JSON array of pool configs', () => {
    const stored =
      '[{"tag":"vip","strategy":"round_robin","rate_limit_policy":"","rate_limit_rest_seconds":0,' +
      '"member_annotations":{"12":{"free":true},"12:3":{"auth_weight":2}}}]'
    expect(parsePoolsOptionValue(stored)).toEqual([
      {
        tag: 'vip',
        strategy: 'round_robin',
        rate_limit_policy: '',
        rate_limit_rest_seconds: 0,
        member_annotations: {
          '12': { free: true },
          '12:3': { auth_weight: 2 },
        },
      },
    ])
  })

  test('returns an empty list for blank, malformed, or non-array payloads', () => {
    expect(parsePoolsOptionValue(undefined)).toEqual([])
    expect(parsePoolsOptionValue('')).toEqual([])
    expect(parsePoolsOptionValue('not json')).toEqual([])
    expect(parsePoolsOptionValue('{"tag":"vip"}')).toEqual([])
    expect(parsePoolsOptionValue('[1,2]')).toEqual([])
  })
})

describe('toPoolFormValue', () => {
  test('pretty-prints member annotations and leaves absent annotations blank', () => {
    expect(
      toPoolFormValue({
        tag: 'free',
        strategy: 'free_first',
        rate_limit_policy: 'move_last',
        rate_limit_rest_seconds: 120,
        member_annotations: { '7': { free: true } },
      })
    ).toEqual({
      tag: 'free',
      strategy: 'free_first',
      rate_limit_policy: 'move_last',
      rate_limit_rest_seconds: 120,
      member_annotations_json: '{\n  "7": {\n    "free": true\n  }\n}',
    })
    expect(
      toPoolFormValue({
        tag: 'paid',
        strategy: 'smart',
        rate_limit_policy: '',
        rate_limit_rest_seconds: 0,
      })
    ).toMatchObject({ member_annotations_json: '' })
  })
})

describe('buildPoolsOptionValue', () => {
  test('round-trips a form entry back into the option JSON, trimming tags and omitting empty annotations', () => {
    const value = buildPoolsOptionValue([
      {
        tag: ' vip ',
        strategy: 'least_used',
        rate_limit_policy: '',
        rate_limit_rest_seconds: 0,
        member_annotations_json: '',
      },
      {
        tag: 'paid',
        strategy: 'fastest',
        rate_limit_policy: 'move_last',
        rate_limit_rest_seconds: 90,
        member_annotations_json: '{"12:3":{"auth_weight":5}}',
      },
    ])
    expect(JSON.parse(value)).toEqual([
      {
        tag: 'vip',
        strategy: 'least_used',
        rate_limit_policy: '',
        rate_limit_rest_seconds: 0,
      },
      {
        tag: 'paid',
        strategy: 'fastest',
        rate_limit_policy: 'move_last',
        rate_limit_rest_seconds: 90,
        member_annotations: { '12:3': { auth_weight: 5 } },
      },
    ])
  })

  test('clamps negative rest seconds to zero', () => {
    const value = buildPoolsOptionValue([
      {
        tag: 'vip',
        strategy: 'ordered',
        rate_limit_policy: '',
        rate_limit_rest_seconds: -5,
        member_annotations_json: '',
      },
    ])
    expect(JSON.parse(value)[0].rate_limit_rest_seconds).toBe(0)
  })
})
