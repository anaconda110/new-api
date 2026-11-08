package service

import (
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/pool_setting"
)

// QuotaCollector reads the quota windows of one pool member from its
// upstream. Implementations must treat a failed read as "unknown", never as
// a dead account: the caller marks quota_unknown and keeps the channel
// serving. Collectors run on a periodic system task, not on the request path.
type QuotaCollector interface {
	Collect(channel *model.Channel, keyIndex int) ([]model.PoolMemberWindow, error)
}

var (
	quotaCollectorsMu     sync.RWMutex
	quotaCollectorsByType = make(map[int]QuotaCollector)
	defaultQuotaCollector QuotaCollector
	poolQuotaWarmupOnce   sync.Once
)

// RegisterQuotaCollector wires a collector for one channel type, overriding
// the default collector for that type. Called from controller init() because
// the balance and console-login collectors live in the controller layer.
func RegisterQuotaCollector(channelType int, collector QuotaCollector) {
	quotaCollectorsMu.Lock()
	defer quotaCollectorsMu.Unlock()
	quotaCollectorsByType[channelType] = collector
}

// RegisterDefaultQuotaCollector wires the fallback collector used when no
// channel-type-specific collector is registered.
func RegisterDefaultQuotaCollector(collector QuotaCollector) {
	quotaCollectorsMu.Lock()
	defer quotaCollectorsMu.Unlock()
	defaultQuotaCollector = collector
}

func quotaCollectorFor(channelType int) QuotaCollector {
	quotaCollectorsMu.RLock()
	defer quotaCollectorsMu.RUnlock()
	if collector, ok := quotaCollectorsByType[channelType]; ok {
		return collector
	}
	return defaultQuotaCollector
}

// HasQuotaCollector reports whether a collector is registered for the channel
// type (including the default balance collector).
func HasQuotaCollector(channelType int) bool {
	return quotaCollectorFor(channelType) != nil
}

// LoadPoolSnapshotsIntoRuntime warms the pool runtime with the persisted
// collector snapshots, so window-aware strategies work right after a restart
// instead of waiting for the next collection run.
func LoadPoolSnapshotsIntoRuntime() {
	snapshots, err := model.GetAllPoolMemberSnapshots()
	if err != nil {
		common.SysError(common.LogText("failed to load pool member snapshots: %v", err))
		return
	}
	for _, snapshot := range snapshots {
		if _, ok := pool_setting.GetPoolByTag(snapshot.Tag); !ok {
			continue
		}
		windows := make([]WindowSnapshot, 0, len(snapshot.Windows))
		for _, window := range snapshot.Windows {
			windows = append(windows, WindowSnapshot{
				Remaining:   window.Remaining,
				Capacity:    window.Capacity,
				ExpiryAt:    window.ExpiryAt,
				ShortWindow: window.ShortWindow,
				Source:      window.Source,
			})
		}
		SetMemberWindows(snapshot.Tag, MemberId{ChannelId: snapshot.ChannelId, KeyIndex: snapshot.KeyIndex}, windows, snapshot.QuotaUnknown)
	}
}

// WarmPoolSnapshots loads the persisted snapshots once at startup.
func WarmPoolSnapshots() {
	poolQuotaWarmupOnce.Do(LoadPoolSnapshotsIntoRuntime)
}

// RunPoolQuotaCollection collects fresh quota windows for every pool with a
// collector and persists them. Collection failures only mark the member's
// quota unknown; they must never disable a channel or a key.
func RunPoolQuotaCollection() int {
	setting := pool_setting.GetPoolSetting()
	if !setting.Enabled || len(setting.Pools) == 0 {
		return 0
	}
	collected := 0
	for _, pool := range setting.Pools {
		channels, err := model.GetChannelsByTag(pool.Tag, false, true)
		if err != nil {
			common.SysError(common.LogText("pool %s: failed to list channels: %v", pool.Tag, err))
			continue
		}
		for _, channel := range channels {
			if channel.Status != common.ChannelStatusEnabled {
				continue
			}
			for _, member := range ExpandPoolMembers(channel) {
				collector := quotaCollectorFor(channel.Type)
				if collector == nil {
					continue
				}
				windows, err := collector.Collect(channel, member.KeyIndex)
				unknown := err != nil
				if unknown {
					common.SysError(common.LogText(
						"pool %s: quota collection failed for member %s (channel #%d, marked unknown, channel stays enabled): %v",
						pool.Tag, member.String(), channel.Id, err,
					))
				}
				snapshots := make([]model.PoolMemberWindow, 0, len(windows))
				for _, window := range windows {
					snapshots = append(snapshots, model.PoolMemberWindow{
						Remaining:   window.Remaining,
						Capacity:    window.Capacity,
						ExpiryAt:    window.ExpiryAt,
						ShortWindow: window.ShortWindow,
						Source:      window.Source,
					})
				}
				SetMemberWindows(pool.Tag, member, toServiceWindows(snapshots), unknown)
				if storeErr := model.UpsertPoolMemberSnapshot(&model.PoolMemberSnapshot{
					ChannelId:    channel.Id,
					KeyIndex:     member.KeyIndex,
					Tag:          pool.Tag,
					Windows:      snapshots,
					QuotaUnknown: unknown,
				}); storeErr != nil {
					common.SysError(common.LogText("pool %s: failed to persist snapshot for member %s: %v", pool.Tag, member.String(), storeErr))
				}
				collected++
				// Collection runs in the background; keep the per-member pace
				// gentle so upstream console endpoints are not hammered.
				time.Sleep(200 * time.Millisecond)
			}
		}
	}
	return collected
}

func toServiceWindows(windows []model.PoolMemberWindow) []WindowSnapshot {
	serviceWindows := make([]WindowSnapshot, 0, len(windows))
	for _, window := range windows {
		serviceWindows = append(serviceWindows, WindowSnapshot{
			Remaining:   window.Remaining,
			Capacity:    window.Capacity,
			ExpiryAt:    window.ExpiryAt,
			ShortWindow: window.ShortWindow,
			Source:      window.Source,
		})
	}
	return serviceWindows
}

// PoolQuotaCollectionSummary is the admin-facing result of one collection run.
type PoolQuotaCollectionSummary struct {
	Collected int `json:"collected"`
}

// RunPoolQuotaCollectionForced is the manual trigger used by the admin API.
func RunPoolQuotaCollectionForced() PoolQuotaCollectionSummary {
	return PoolQuotaCollectionSummary{Collected: RunPoolQuotaCollection()}
}
