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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2, Plus, RefreshCw, Trash2, Waypoints } from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import {
  useFieldArray,
  useForm,
  type Resolver,
  type UseFormReturn,
} from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import {
  StaticDataTable,
  type StaticDataTableColumn,
} from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { JsonCodeEditor } from '@/components/json-code-editor'
import { StatusBadge } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Separator } from '@/components/ui/separator'
import { Switch } from '@/components/ui/switch'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import {
  getSystemOptions,
  updateSystemOption,
} from '@/features/system-settings/api'
import { getOptionValue } from '@/features/system-settings/hooks/use-system-options'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { formatNumber, formatPercent } from '@/lib/format'
import { handleServerError } from '@/lib/handle-server-error'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { collectPoolQuota, getPoolStatus } from '../../api'
import {
  CHANNEL_STATUS,
  CHANNEL_STATUS_LABELS,
  POOL_DEFAULT_QUOTA_REFRESH_MINUTES,
  POOL_RATE_LIMIT_POLICY_LABELS,
  POOL_STRATEGIES,
  POOL_STRATEGY_LABELS,
} from '../../constants'
import { channelsQueryKeys } from '../../lib'
import {
  buildPoolsOptionValue,
  parsePoolsOptionValue,
  toPoolFormValue,
  type PoolFormPool,
} from '../../lib/pool-config'
import type { PoolMemberStatus, PoolStatus, PoolStrategy } from '../../types'

type PoolDialogTab = 'status' | 'config'

interface PoolFormValues {
  enabled: boolean
  quota_refresh_minutes: number
  pools: PoolFormPool[]
}

type PoolSchedulingDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
}

const POOL_FORM_ID = 'pool-scheduling-form'

// Error messages are i18n keys (English source strings); render them through
// t() at display time.
const POOL_FORM_ERROR_KEYS = {
  TAG_REQUIRED: 'Tag is required',
  INVALID_NUMBER: 'Enter a valid number',
  INVALID_INTEGER: 'Enter a whole number',
  NEGATIVE: 'Must not be negative',
  INVALID_ANNOTATIONS_JSON: 'Invalid JSON in member annotations',
  ANNOTATIONS_NOT_OBJECT: 'Member annotations must be a JSON object',
  ANNOTATION_ENTRY_NOT_OBJECT: 'Each member annotation must be a JSON object',
} as const

const memberAnnotationsJsonSchema = z.string().superRefine((value, ctx) => {
  const trimmed = value.trim()
  if (!trimmed) return
  let parsed: unknown
  try {
    parsed = JSON.parse(trimmed)
  } catch {
    ctx.addIssue({
      code: 'custom',
      message: POOL_FORM_ERROR_KEYS.INVALID_ANNOTATIONS_JSON,
    })
    return
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    ctx.addIssue({
      code: 'custom',
      message: POOL_FORM_ERROR_KEYS.ANNOTATIONS_NOT_OBJECT,
    })
    return
  }
  const invalidEntry = Object.values(parsed).some(
    (annotation) =>
      typeof annotation !== 'object' ||
      annotation === null ||
      Array.isArray(annotation)
  )
  if (invalidEntry) {
    ctx.addIssue({
      code: 'custom',
      message: POOL_FORM_ERROR_KEYS.ANNOTATION_ENTRY_NOT_OBJECT,
    })
  }
})

const nonNegativeIntSchema = z.coerce
  .number({ message: POOL_FORM_ERROR_KEYS.INVALID_NUMBER })
  .int(POOL_FORM_ERROR_KEYS.INVALID_INTEGER)
  .min(0, POOL_FORM_ERROR_KEYS.NEGATIVE)

const poolFormSchema = z.object({
  enabled: z.boolean(),
  quota_refresh_minutes: nonNegativeIntSchema,
  pools: z.array(
    z.object({
      tag: z.string().trim().min(1, POOL_FORM_ERROR_KEYS.TAG_REQUIRED),
      strategy: z.enum(POOL_STRATEGIES),
      rate_limit_policy: z.enum(['', 'keep', 'move_last']),
      rate_limit_rest_seconds: nonNegativeIntSchema,
      member_annotations_json: memberAnnotationsJsonSchema,
    })
  ),
})

/**
 * Re-render once per second while there is at least one resting member so
 * the resting countdown stays current without a refetch.
 */
function useNowTick(active: boolean) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!active) return
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [active])
  return now
}

export function PoolSchedulingDialog(props: PoolSchedulingDialogProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const currentUser = useAuthStore((s) => s.auth.user)
  const canOperate = hasPermission(
    currentUser,
    ADMIN_PERMISSION_RESOURCES.CHANNEL,
    ADMIN_PERMISSION_ACTIONS.OPERATE
  )
  const [activeTab, setActiveTab] = useState<PoolDialogTab>('status')

  const poolStatusQuery = useQuery({
    queryKey: channelsQueryKeys.poolStatus(),
    queryFn: async () => {
      const response = await getPoolStatus()
      return requireServerSuccess(response).data ?? { pools: [] }
    },
    enabled: props.open,
  })

  const systemOptionsQuery = useQuery({
    queryKey: ['system-options'],
    queryFn: async () => requireServerSuccess(await getSystemOptions()),
    enabled: props.open,
  })

  const collectMutation = useMutation({
    mutationFn: async () => requireServerSuccess(await collectPoolQuota()),
    onSuccess: (response) => {
      toast.success(
        t('Collected quota for {{count}} members', {
          count: response.data?.collected ?? 0,
        })
      )
      void poolStatusQuery.refetch()
    },
    onError: (error) =>
      handleServerError(error, t('Failed to collect pool quota')),
  })

  const form = useForm<PoolFormValues>({
    resolver: zodResolver(
      poolFormSchema
    ) as unknown as Resolver<PoolFormValues>,
    defaultValues: {
      enabled: false,
      quota_refresh_minutes: POOL_DEFAULT_QUOTA_REFRESH_MINUTES,
      pools: [],
    },
  })

  // Load pool settings into the form when the dialog opens (or when the
  // stored options change). The serialized key guards against clobbering
  // in-progress edits on unrelated background refetches.
  const loadedOptionsKeyRef = useRef('')
  const optionsData = systemOptionsQuery.data?.data
  useEffect(() => {
    if (!props.open || !optionsData) return
    const optionValues = getOptionValue(optionsData, {
      'pool_setting.enabled': false,
      'pool_setting.quota_refresh_minutes': POOL_DEFAULT_QUOTA_REFRESH_MINUTES,
    })
    const pools = parsePoolsOptionValue(
      optionsData.find((option) => option.key === 'pool_setting.pools')?.value
    )
    const loadedKey = [
      optionValues['pool_setting.enabled'],
      optionValues['pool_setting.quota_refresh_minutes'],
      JSON.stringify(pools),
    ].join('\n')
    if (loadedOptionsKeyRef.current === loadedKey) return
    loadedOptionsKeyRef.current = loadedKey
    form.reset({
      enabled: optionValues['pool_setting.enabled'],
      quota_refresh_minutes: optionValues['pool_setting.quota_refresh_minutes'],
      pools: pools.map(toPoolFormValue),
    })
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.open, optionsData])

  const saveMutation = useMutation({
    mutationFn: async (values: PoolFormValues) => {
      const updates = [
        { key: 'pool_setting.enabled', value: String(values.enabled) },
        {
          key: 'pool_setting.quota_refresh_minutes',
          value: String(
            Math.max(0, Math.floor(values.quota_refresh_minutes || 0))
          ),
        },
        {
          key: 'pool_setting.pools',
          value: buildPoolsOptionValue(values.pools),
        },
      ]
      for (const update of updates) {
        await requireServerSuccess(await updateSystemOption(update))
      }
    },
    onSuccess: (_data, variables) => {
      toast.success(t('Pool settings saved'))
      form.reset(variables)
      queryClient.invalidateQueries({ queryKey: ['system-options'] })
      queryClient.invalidateQueries({
        queryKey: channelsQueryKeys.poolStatus(),
      })
    },
    onError: (error) =>
      handleServerError(error, t('Failed to save pool settings')),
  })

  const pools = useMemo(
    () => poolStatusQuery.data?.pools ?? [],
    [poolStatusQuery.data]
  )

  return (
    <Dialog
      open={props.open}
      onOpenChange={props.onOpenChange}
      title={
        <>
          <Waypoints className='h-4 w-4' aria-hidden />
          {t('Pool Scheduling')}
        </>
      }
      description={t('View pool member status and configure pool scheduling')}
      contentClassName='flex max-h-[min(90dvh,var(--dialog-available-height))] max-w-5xl flex-col'
      titleClassName='flex items-center gap-2'
      contentHeight='min(72vh, 720px)'
      bodyClassName='space-y-4'
      footer={
        activeTab === 'config' ? (
          <>
            <Button
              type='button'
              variant='outline'
              onClick={() => props.onOpenChange(false)}
            >
              {t('Cancel')}
            </Button>
            <Button
              type='submit'
              form={POOL_FORM_ID}
              disabled={saveMutation.isPending}
            >
              {t('Save')}
            </Button>
          </>
        ) : undefined
      }
    >
      <Tabs
        value={activeTab}
        onValueChange={(value) => {
          if (value !== null) setActiveTab(value as PoolDialogTab)
        }}
        className='flex min-h-0 flex-1 flex-col gap-3'
      >
        <TabsList className='shrink-0'>
          <TabsTrigger value='status' className='px-3'>
            {t('Status')}
          </TabsTrigger>
          <TabsTrigger value='config' className='px-3'>
            {t('Configuration')}
          </TabsTrigger>
        </TabsList>
        <TabsContent value='status' className='min-h-0 flex-1'>
          <PoolStatusPanel
            pools={pools}
            isLoading={poolStatusQuery.isLoading}
            isFetching={poolStatusQuery.isFetching}
            isCollecting={collectMutation.isPending}
            canCollect={canOperate}
            onRefresh={() => void poolStatusQuery.refetch()}
            onCollect={() => collectMutation.mutate()}
          />
        </TabsContent>
        <TabsContent value='config' className='min-h-0 flex-1'>
          <PoolConfigPanel
            form={form}
            isLoadingOptions={systemOptionsQuery.isLoading}
            onSubmit={(values) => saveMutation.mutate(values)}
          />
        </TabsContent>
      </Tabs>
    </Dialog>
  )
}

function PoolStatusPanel(props: {
  pools: PoolStatus[]
  isLoading: boolean
  isFetching: boolean
  isCollecting: boolean
  canCollect: boolean
  onRefresh: () => void
  onCollect: () => void
}) {
  const { t } = useTranslation()
  const hasResting = props.pools.some((pool) =>
    pool.members.some((member) => member.resting_until)
  )
  const now = useNowTick(props.pools.length > 0 && hasResting)

  const memberColumns = useMemo<StaticDataTableColumn<PoolMemberStatus>[]>(
    () => [
      {
        id: 'member',
        header: t('Member'),
        className: 'w-24',
        cellClassName: 'font-mono text-xs',
        cell: (member) => (
          <div className='flex items-center gap-1.5'>
            <span>{member.member}</span>
            {member.annotation?.free && (
              <StatusBadge
                label={t('Free')}
                variant='success'
                copyable={false}
              />
            )}
          </div>
        ),
      },
      {
        id: 'channel',
        header: t('Channel'),
        className: 'min-w-[140px]',
        cell: (member) => (
          <div className='flex items-center gap-1.5'>
            <span className='truncate text-sm'>{member.channel_name}</span>
            {member.channel_status !== CHANNEL_STATUS.ENABLED && (
              <StatusBadge
                label={t(
                  (CHANNEL_STATUS_LABELS as Record<number, string>)[
                    member.channel_status
                  ] ?? 'Unknown'
                )}
                variant='danger'
                copyable={false}
              />
            )}
          </div>
        ),
      },
      {
        id: 'windows',
        header: t('Window Usage'),
        className: 'min-w-[180px]',
        cell: (member) => {
          const windows = member.windows ?? []
          if (windows.length === 0 && !member.quota_unknown) {
            return <span className='text-muted-foreground text-xs'>-</span>
          }
          return (
            <div className='flex flex-col gap-1'>
              {windows.map((window) => {
                const hasCapacity =
                  window.capacity !== undefined && window.capacity > 0
                return (
                  <div
                    key={`${window.source ?? 'w'}:${window.remaining}:${window.capacity ?? ''}:${window.expiry_at ?? ''}`}
                    className='flex flex-wrap items-center gap-1.5'
                  >
                    <span className='text-xs tabular-nums'>
                      {hasCapacity
                        ? `${formatNumber(window.remaining)}/${formatNumber(window.capacity)}`
                        : formatNumber(window.remaining)}
                    </span>
                    {hasCapacity && (
                      <span className='text-muted-foreground text-xs tabular-nums'>
                        {formatPercent(
                          ((window.remaining ?? 0) / (window.capacity ?? 1)) *
                            100
                        )}
                      </span>
                    )}
                    {window.short_window && (
                      <StatusBadge
                        label={t('Short Window')}
                        variant='info'
                        copyable={false}
                      />
                    )}
                  </div>
                )
              })}
              {member.quota_unknown && (
                <StatusBadge
                  label={t('Unknown')}
                  variant='warning'
                  copyable={false}
                />
              )}
            </div>
          )
        },
      },
      {
        id: 'resting',
        header: t('Resting'),
        className: 'w-28',
        cell: (member) => {
          if (!member.resting_until) {
            return <span className='text-muted-foreground text-xs'>-</span>
          }
          const seconds = Math.max(
            0,
            Math.ceil((new Date(member.resting_until).getTime() - now) / 1000)
          )
          return (
            <span className='text-xs tabular-nums'>
              {t('Resting {{seconds}}s', { seconds })}
            </span>
          )
        },
      },
      {
        id: 'latency',
        header: t('Latency'),
        className: 'w-24',
        cellClassName: 'text-xs tabular-nums',
        cell: (member) =>
          member.latency_ms > 0 ? `${formatNumber(member.latency_ms)} ms` : '-',
      },
      {
        id: 'used-requests',
        header: t('Used Requests'),
        className: 'w-28',
        cellClassName: 'text-xs tabular-nums',
        cell: (member) => formatNumber(member.used_requests),
      },
      {
        id: 'penalty',
        header: t('Penalty'),
        className: 'w-20',
        cellClassName: 'text-xs tabular-nums',
        cell: (member) => formatNumber(member.penalty),
      },
    ],
    [t, now]
  )

  if (props.isLoading) {
    return (
      <div className='flex items-center justify-center py-12'>
        <Loader2 className='text-muted-foreground h-8 w-8 animate-spin' />
      </div>
    )
  }

  return (
    <div className='flex min-h-0 flex-1 flex-col space-y-4 overflow-hidden'>
      <div className='flex shrink-0 items-center justify-end gap-2'>
        <Button
          variant='outline'
          size='sm'
          onClick={props.onRefresh}
          disabled={props.isFetching}
          aria-label={t('Refresh')}
        >
          <RefreshCw
            className={props.isFetching ? 'h-4 w-4 animate-spin' : 'h-4 w-4'}
          />
        </Button>
        <Button
          variant='outline'
          size='sm'
          onClick={props.onCollect}
          disabled={props.isCollecting || !props.canCollect}
          title={
            props.canCollect
              ? undefined
              : t('No permission to perform this action')
          }
        >
          {props.isCollecting ? (
            <Loader2 className='h-4 w-4 animate-spin' />
          ) : (
            <Waypoints className='h-4 w-4' />
          )}
          {t('Collect Now')}
        </Button>
      </div>

      <div className='min-h-0 flex-1 space-y-4 overflow-auto rounded-md'>
        {props.pools.length === 0 ? (
          <div className='text-muted-foreground py-12 text-center'>
            <div>{t('No pools configured')}</div>
            <p className='mt-2 text-xs'>
              {t(
                'Enable pool scheduling and configure at least one pool to see member status'
              )}
            </p>
          </div>
        ) : (
          props.pools.map((pool) => (
            <div key={pool.tag} className='space-y-2'>
              <div className='flex flex-wrap items-center gap-2'>
                <StatusBadge
                  label={pool.tag}
                  variant='neutral'
                  copyable={false}
                />
                <span className='text-muted-foreground text-xs'>
                  {t(POOL_STRATEGY_LABELS[pool.strategy] ?? pool.strategy)}
                </span>
                <span className='text-muted-foreground text-xs'>
                  {pool.members.length}{' '}
                  {pool.members.length === 1 ? t('Member') : t('Members')}
                </span>
              </div>
              <div className='overflow-hidden rounded-md border'>
                <StaticDataTable
                  className='rounded-none border-0'
                  tableClassName='min-w-[820px]'
                  data={pool.members}
                  getRowKey={(member) => member.member}
                  columns={memberColumns}
                  emptyContent={
                    <span className='text-muted-foreground'>
                      {t('No members in this pool')}
                    </span>
                  }
                />
              </div>
            </div>
          ))
        )}
      </div>
    </div>
  )
}

function PoolConfigPanel(props: {
  form: UseFormReturn<PoolFormValues>
  isLoadingOptions: boolean
  onSubmit: (values: PoolFormValues) => void
}) {
  const { t } = useTranslation()
  const form = props.form
  const { fields, append, remove } = useFieldArray({
    control: form.control,
    name: 'pools',
  })
  const poolErrors = form.formState.errors.pools
  const enabled = form.watch('enabled')

  const strategyItems = POOL_STRATEGIES.map((strategy) => ({
    value: strategy,
    label: t(POOL_STRATEGY_LABELS[strategy]),
  }))
  const policyItems = (['keep', 'move_last'] as const).map((policy) => ({
    value: policy,
    label: t(POOL_RATE_LIMIT_POLICY_LABELS[policy]),
  }))

  if (props.isLoadingOptions) {
    return (
      <div className='flex items-center justify-center py-12'>
        <Loader2 className='text-muted-foreground h-8 w-8 animate-spin' />
      </div>
    )
  }

  return (
    <form
      id={POOL_FORM_ID}
      onSubmit={form.handleSubmit(props.onSubmit)}
      className='space-y-4'
    >
      <div className='flex items-center justify-between'>
        <div className='space-y-0.5'>
          <Label htmlFor='pool-scheduling-enabled'>
            {t('Enable Pool Scheduling')}
          </Label>
          <p className='text-muted-foreground text-xs'>
            {t('Schedule channels with the same tag as one account pool')}
          </p>
        </div>
        <Switch
          id='pool-scheduling-enabled'
          checked={enabled}
          onCheckedChange={(checked) =>
            form.setValue('enabled', checked, { shouldDirty: true })
          }
        />
      </div>

      <div className='grid gap-1.5'>
        <Label htmlFor='pool-quota-refresh-minutes'>
          {t('Quota Refresh Minutes')}
        </Label>
        <Input
          id='pool-quota-refresh-minutes'
          type='number'
          min={0}
          {...form.register('quota_refresh_minutes')}
        />
        <p className='text-muted-foreground text-xs'>
          {t('0 uses the server default')}
        </p>
        {form.formState.errors.quota_refresh_minutes?.message && (
          <p className='text-destructive text-xs'>
            {t(form.formState.errors.quota_refresh_minutes.message)}
          </p>
        )}
      </div>

      <Separator />

      <div>
        <div className='mb-2 flex items-center justify-between'>
          <Label>{t('Pools')}</Label>
          <Button
            type='button'
            variant='outline'
            size='sm'
            onClick={() =>
              append({
                tag: '',
                strategy: 'smart',
                rate_limit_policy: '',
                rate_limit_rest_seconds: 0,
                member_annotations_json: '',
              })
            }
          >
            <Plus className='mr-1 h-3 w-3' />
            {t('Add Pool')}
          </Button>
        </div>
        {fields.length === 0 && (
          <p className='text-muted-foreground text-sm'>
            {t('No pools configured')}
          </p>
        )}
        <div className='space-y-3'>
          {fields.map((field, index) => {
            const poolError = poolErrors?.[index]
            const strategyValue = form.watch(`pools.${index}.strategy`)
            const policyValue = form.watch(`pools.${index}.rate_limit_policy`)
            const annotationsJson = form.watch(
              `pools.${index}.member_annotations_json`
            )
            return (
              <div key={field.id} className='space-y-3 rounded-lg border p-3'>
                <div className='grid gap-3 sm:grid-cols-2'>
                  <div className='grid gap-1.5'>
                    <Label required>{t('Tag')}</Label>
                    <Input
                      placeholder='vip'
                      {...form.register(`pools.${index}.tag`)}
                    />
                    {poolError?.tag?.message && (
                      <p className='text-destructive text-xs'>
                        {t(poolError.tag.message)}
                      </p>
                    )}
                  </div>
                  <div className='grid gap-1.5'>
                    <Label>{t('Strategy')}</Label>
                    <Select
                      items={strategyItems}
                      value={strategyValue}
                      onValueChange={(value) => {
                        if (value === null) return
                        form.setValue(
                          `pools.${index}.strategy`,
                          value as PoolStrategy,
                          { shouldDirty: true }
                        )
                      }}
                    >
                      <SelectTrigger className='w-full'>
                        <SelectValue placeholder={t('Strategy')} />
                      </SelectTrigger>
                      <SelectContent alignItemWithTrigger={false}>
                        <SelectGroup>
                          {strategyItems.map((item) => (
                            <SelectItem key={item.value} value={item.value}>
                              {item.label}
                            </SelectItem>
                          ))}
                        </SelectGroup>
                      </SelectContent>
                    </Select>
                    {poolError?.strategy?.message && (
                      <p className='text-destructive text-xs'>
                        {t(poolError.strategy.message)}
                      </p>
                    )}
                  </div>
                  <div className='grid gap-1.5'>
                    <Label>{t('Rate Limit Policy')}</Label>
                    <Select
                      items={policyItems}
                      value={policyValue === 'move_last' ? 'move_last' : 'keep'}
                      onValueChange={(value) => {
                        if (value === null) return
                        form.setValue(
                          `pools.${index}.rate_limit_policy`,
                          value === 'move_last' ? 'move_last' : '',
                          { shouldDirty: true }
                        )
                      }}
                    >
                      <SelectTrigger className='w-full'>
                        <SelectValue placeholder={t('Rate Limit Policy')} />
                      </SelectTrigger>
                      <SelectContent alignItemWithTrigger={false}>
                        <SelectGroup>
                          {policyItems.map((item) => (
                            <SelectItem key={item.value} value={item.value}>
                              {item.label}
                            </SelectItem>
                          ))}
                        </SelectGroup>
                      </SelectContent>
                    </Select>
                    <p className='text-muted-foreground text-xs'>
                      {t('What happens to a member after an upstream 429')}
                    </p>
                  </div>
                  <div className='grid gap-1.5'>
                    <Label htmlFor={`pool-rest-seconds-${field.id}`}>
                      {t('Rate Limit Rest Seconds')}
                    </Label>
                    <Input
                      id={`pool-rest-seconds-${field.id}`}
                      type='number'
                      min={0}
                      {...form.register(
                        `pools.${index}.rate_limit_rest_seconds`
                      )}
                    />
                    <p className='text-muted-foreground text-xs'>
                      {t('0 uses the server default')}
                    </p>
                    {poolError?.rate_limit_rest_seconds?.message && (
                      <p className='text-destructive text-xs'>
                        {t(poolError.rate_limit_rest_seconds.message)}
                      </p>
                    )}
                  </div>
                </div>

                <div className='grid gap-1.5'>
                  <Label>{t('Member Annotations (JSON)')}</Label>
                  <p className='text-muted-foreground text-xs'>
                    {t(
                      'Per-member hints keyed by channel id ("12") or channel id and key index ("12:3")'
                    )}
                  </p>
                  <JsonCodeEditor
                    ariaLabel={t('Member Annotations (JSON)')}
                    value={annotationsJson || ''}
                    onChange={(value) =>
                      form.setValue(
                        `pools.${index}.member_annotations_json`,
                        value,
                        { shouldDirty: true }
                      )
                    }
                    placeholder='{"12": {"free": true, "manual_windows": [{"remaining": 100, "expiry_hours": 5}]}}'
                    heightClassName='h-36 min-h-36 max-h-36'
                    aria-invalid={Boolean(poolError?.member_annotations_json)}
                  />
                  {poolError?.member_annotations_json?.message && (
                    <p className='text-destructive text-xs'>
                      {t(poolError.member_annotations_json.message)}
                    </p>
                  )}
                </div>

                <div className='flex justify-end'>
                  <Button
                    type='button'
                    variant='ghost'
                    size='icon'
                    onClick={() => remove(index)}
                    aria-label={t('Remove')}
                  >
                    <Trash2 className='h-4 w-4' />
                  </Button>
                </div>
              </div>
            )
          })}
        </div>
      </div>
    </form>
  )
}
