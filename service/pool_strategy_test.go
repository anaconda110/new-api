package service

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/pool_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupPoolSetting(t *testing.T, pools ...pool_setting.PoolConfig) {
	t.Helper()
	original := *pool_setting.GetPoolSetting()
	pool_setting.GetPoolSetting().Enabled = true
	pool_setting.GetPoolSetting().Pools = pools
	t.Cleanup(func() {
		*pool_setting.GetPoolSetting() = original
	})
}

func poolTestChannel(id int, tag string, weight uint, keys []string, disabledKeys map[int]int) *model.Channel {
	channel := &model.Channel{
		Id:     id,
		Name:   fmt.Sprintf("pool-test-%d", id),
		Status: common.ChannelStatusEnabled,
	}
	channel.SetTag(tag)
	channel.Weight = &weight
	if len(keys) == 1 {
		channel.Key = keys[0]
	} else {
		channel.Key = strings.Join(keys, "\n")
		channel.ChannelInfo = model.ChannelInfo{
			IsMultiKey:         true,
			MultiKeySize:       len(keys),
			MultiKeyStatusList: disabledKeys,
		}
	}
	return channel
}

func poolCand(channelId, keyIndex int) poolCandidate {
	return poolCandidate{
		Member:  MemberId{ChannelId: channelId, KeyIndex: keyIndex},
		Channel: poolTestChannel(channelId, "t", 0, []string{"k"}, nil),
	}
}

func poolWindow(remaining, capacity float64, expiryHours float64, short bool) WindowSnapshot {
	snapshot := WindowSnapshot{Remaining: remaining, Capacity: capacity, ShortWindow: short}
	if expiryHours > 0 {
		snapshot.ExpiryAt = time.Now().Add(time.Duration(expiryHours * float64(time.Hour)))
	}
	return snapshot
}

func TestPoolOrderedSelectsLowestChannelAndSinksPenalized(t *testing.T) {
	candidates := []poolCandidate{poolCand(3, -1), poolCand(1, -1), poolCand(2, -1)}

	selected := selectOrdered(candidates)
	require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, selected.Member)

	// move_last: the penalized member sinks behind the healthy ones.
	candidates[1].Penalty = 2
	selected = selectOrdered(candidates)
	require.Equal(t, MemberId{ChannelId: 2, KeyIndex: -1}, selected.Member)

	// When every member is penalized the relative order is preserved.
	for i := range candidates {
		candidates[i].Penalty = 1
	}
	selected = selectOrdered(candidates)
	require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, selected.Member)
}

func TestPoolExhaustThenSwitchSticksUntilRest(t *testing.T) {
	tag := "pool-test-exhaust"
	setupPoolSetting(t, pool_setting.PoolConfig{
		Tag:      tag,
		Strategy: pool_setting.StrategyExhaustThenSwitch,
	})
	candidates := []poolCandidate{poolCand(1, -1), poolCand(2, -1)}
	for i := range candidates {
		candidates[i].Member = MemberId{ChannelId: candidates[i].Member.ChannelId, KeyIndex: -1}
	}

	first := selectExhaustThenSwitch(pool_setting.PoolConfig{Tag: tag}, candidates, nil)
	require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, first.Member)

	// The same member is reused while it stays healthy.
	second := selectExhaustThenSwitch(pool_setting.PoolConfig{Tag: tag}, candidates, nil)
	require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, second.Member)

	// Once the sticky member rests the strategy moves on and re-sticks. The
	// strategies receive pre-filtered candidates from selectPoolMember, so the
	// test applies the same filter after marking the rest.
	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 1, KeyIndex: -1}, time.Now().Add(time.Minute), false)
	available := filterAvailableMembers(pool_setting.PoolConfig{Tag: tag}, candidates, nil)
	require.Len(t, available, 1)
	third := selectExhaustThenSwitch(pool_setting.PoolConfig{Tag: tag}, available, nil)
	require.Equal(t, MemberId{ChannelId: 2, KeyIndex: -1}, third.Member)
	fourth := selectExhaustThenSwitch(pool_setting.PoolConfig{Tag: tag}, available, nil)
	require.Equal(t, MemberId{ChannelId: 2, KeyIndex: -1}, fourth.Member)
}

func TestPoolRoundRobinRotates(t *testing.T) {
	tag := "pool-test-rr"
	candidates := []poolCandidate{poolCand(1, -1), poolCand(2, -1), poolCand(3, -1)}
	pool := pool_setting.PoolConfig{Tag: tag}

	seen := make([]int, 0, 4)
	for i := 0; i < 4; i++ {
		selected := selectRoundRobin(pool, candidates)
		seen = append(seen, selected.Member.ChannelId)
	}
	assert.Equal(t, []int{1, 2, 3, 1}, seen)
}

func TestPoolLeastUsedPrefersLowestWindowUsage(t *testing.T) {
	a, b := poolCand(1, -1), poolCand(2, -1)
	a.Windows = []WindowSnapshot{poolWindow(10, 100, 0, false)} // 90% used
	b.Windows = []WindowSnapshot{poolWindow(50, 100, 0, false)} // 50% used
	selected := selectLeastUsed([]poolCandidate{a, b})
	require.Equal(t, MemberId{ChannelId: 2, KeyIndex: -1}, selected.Member)

	// Without window capacity the observation counter decides.
	c, d := poolCand(1, -1), poolCand(2, -1)
	c.UsedReqs = 5
	d.UsedReqs = 0
	selected = selectLeastUsed([]poolCandidate{c, d})
	require.Equal(t, MemberId{ChannelId: 2, KeyIndex: -1}, selected.Member)
}

func TestPoolUseBeforeExpirySpendsSoonestExpiryFirst(t *testing.T) {
	// Same remaining, sooner expiry wins: 40/1h beats 40/10h.
	a, b := poolCand(1, -1), poolCand(2, -1)
	a.Windows = []WindowSnapshot{poolWindow(40, 100, 1, false)}
	b.Windows = []WindowSnapshot{poolWindow(40, 100, 10, false)}
	selected := selectUseBeforeExpiry([]poolCandidate{a, b})
	require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, selected.Member)

	// Less remaining but far more urgent still wins: 5/0.5h beats 90/10h.
	c, d := poolCand(1, -1), poolCand(2, -1)
	c.Windows = []WindowSnapshot{poolWindow(5, 100, 0.5, false)}
	d.Windows = []WindowSnapshot{poolWindow(90, 100, 10, false)}
	selected = selectUseBeforeExpiry([]poolCandidate{c, d})
	require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, selected.Member)
}

func TestPoolUseBeforeExpiryHoldbackShortWindow(t *testing.T) {
	// A short window at >= 90% usage is parked while a healthy member exists.
	held := poolCand(1, -1)
	held.Windows = []WindowSnapshot{poolWindow(5, 100, 2, true)}
	healthy := poolCand(2, -1)
	healthy.Windows = []WindowSnapshot{poolWindow(50, 100, 2, true)}
	selected := selectUseBeforeExpiry([]poolCandidate{held, healthy})
	require.Equal(t, MemberId{ChannelId: 2, KeyIndex: -1}, selected.Member)

	// When every member is held back the park is lifted (phase two).
	held2 := poolCand(1, -1)
	held2.Windows = []WindowSnapshot{poolWindow(5, 100, 2, true)}
	held3 := poolCand(2, -1)
	held3.Windows = []WindowSnapshot{poolWindow(8, 100, 2, true)}
	selected = selectUseBeforeExpiry([]poolCandidate{held2, held3})
	require.NotNil(t, selected)
	assert.Contains(t, []int{1, 2}, selected.Member.ChannelId)

	// Weekly-style long windows at the same usage are not parked, but the
	// score still prefers the member with the most remaining per hour until
	// expiry (50/2h beats 5/168h).
	weekly := poolCand(1, -1)
	weekly.Windows = []WindowSnapshot{poolWindow(5, 100, 24*7, false)}
	selected = selectUseBeforeExpiry([]poolCandidate{weekly, healthy})
	require.Equal(t, MemberId{ChannelId: 2, KeyIndex: -1}, selected.Member)
}

func TestPoolAuthWeightPrefersHeavierMember(t *testing.T) {
	heavy := poolCand(1, -1)
	heavy.Channel.Weight = common.GetPointer[uint](10)
	light := poolCand(2, -1)
	light.Channel.Weight = common.GetPointer[uint](0)

	for i := 0; i < 20; i++ {
		selected := selectAuthWeight([]poolCandidate{heavy, light}, 0, 2)
		require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, selected.Member)
	}

	// All-zero weights fall back to uniform: any member may win.
	zero1, zero2 := poolCand(1, -1), poolCand(2, -1)
	selected := selectAuthWeight([]poolCandidate{zero1, zero2}, 0, 2)
	require.NotNil(t, selected)
}

func TestPoolFreeFirstPartitions(t *testing.T) {
	free := poolCand(1, -1)
	free.Annotation.Free = true
	paid := poolCand(2, -1)
	paid.Channel.Weight = common.GetPointer[uint](100)

	selected := selectFreeFirst([]poolCandidate{paid, free})
	require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, selected.Member)

	// Without any free annotation the annotation is ignored.
	selected = selectFreeFirst([]poolCandidate{paid, poolCand(3, -1)})
	require.Equal(t, MemberId{ChannelId: 2, KeyIndex: -1}, selected.Member)
}

func TestPoolFastestPrefersLowestLatency(t *testing.T) {
	fast, slow := poolCand(1, -1), poolCand(2, -1)
	fast.LatencyMs = 120
	slow.LatencyMs = 900
	selected := selectFastest([]poolCandidate{fast, slow})
	require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, selected.Member)

	// Without observations the channel test response time decides.
	untested, tested := poolCand(1, -1), poolCand(2, -1)
	tested.Channel.ResponseTime = 80
	selected = selectFastest([]poolCandidate{untested, tested})
	require.Equal(t, MemberId{ChannelId: 2, KeyIndex: -1}, selected.Member)
}

func TestPoolSmartPrefersWindowHeadroom(t *testing.T) {
	rich, poor := poolCand(1, -1), poolCand(2, -1)
	rich.Windows = []WindowSnapshot{poolWindow(90, 100, 0, false)}
	poor.Windows = []WindowSnapshot{poolWindow(5, 100, 0, false)}
	selected := selectSmart([]poolCandidate{rich, poor})
	require.Equal(t, MemberId{ChannelId: 1, KeyIndex: -1}, selected.Member)
}

func TestPoolFilterSkipsUsedAndResting(t *testing.T) {
	tag := "pool-test-filter"
	setupPoolSetting(t, pool_setting.PoolConfig{Tag: tag, Strategy: pool_setting.StrategyOrdered})
	candidates := []poolCandidate{poolCand(1, -1), poolCand(2, -1), poolCand(3, -1)}

	available := filterAvailableMembers(pool_setting.PoolConfig{Tag: tag}, candidates, map[int]bool{1: true})
	require.Len(t, available, 2)
	assert.Equal(t, 2, available[0].Member.ChannelId)

	// Resting members drop out of the candidate set.
	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 2, KeyIndex: -1}, time.Now().Add(time.Minute), false)
	available = filterAvailableMembers(pool_setting.PoolConfig{Tag: tag}, candidates, map[int]bool{1: true})
	require.Len(t, available, 1)
	assert.Equal(t, 3, available[0].Member.ChannelId)

	// With nothing left the filter returns nil and selection falls back.
	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 3, KeyIndex: -1}, time.Now().Add(time.Minute), false)
	assert.Nil(t, filterAvailableMembers(pool_setting.PoolConfig{Tag: tag}, candidates, map[int]bool{1: true}))
}

func TestPoolMarkRestingPolicies(t *testing.T) {
	tag := "pool-test-policy"
	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 1, KeyIndex: -1}, time.Now().Add(time.Minute), false)
	state := poolRuntimeFor(tag).memberRuntimeOf(MemberId{ChannelId: 1, KeyIndex: -1})
	assert.Zero(t, state.penalty, "keep policy must not penalize")
	assert.True(t, state.restingUntil.After(time.Now()))

	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 2, KeyIndex: -1}, time.Now().Add(time.Minute), true)
	state = poolRuntimeFor(tag).memberRuntimeOf(MemberId{ChannelId: 2, KeyIndex: -1})
	assert.Equal(t, 1, state.penalty, "move_last policy must penalize")

	// Clearing the sticky member when it rests keeps exhaust-then-switch sane.
	poolRuntimeFor(tag).setSticky(MemberId{ChannelId: 2, KeyIndex: -1})
	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 2, KeyIndex: -1}, time.Now().Add(time.Minute), false)
	_, stickySet := poolRuntimeFor(tag).stickyMember()
	assert.False(t, stickySet)
}

func TestPoolForTierRequiresUniformTag(t *testing.T) {
	setupPoolSetting(t, pool_setting.PoolConfig{Tag: "samenose", Strategy: pool_setting.StrategyOrdered})

	sameTag := []*model.Channel{
		poolTestChannel(1, "samenose", 0, []string{"k"}, nil),
		poolTestChannel(2, "samenose", 0, []string{"k"}, nil),
	}
	_, ok := poolForTier(sameTag)
	assert.True(t, ok)

	mixed := []*model.Channel{
		poolTestChannel(1, "samenose", 0, []string{"k"}, nil),
		poolTestChannel(2, "other", 0, []string{"k"}, nil),
	}
	_, ok = poolForTier(mixed)
	assert.False(t, ok)

	untagged := []*model.Channel{poolTestChannel(1, "", 0, []string{"k"}, nil)}
	_, ok = poolForTier(untagged)
	assert.False(t, ok)

	// A tag without a pool configuration does not activate scheduling.
	_, ok = poolForTier([]*model.Channel{poolTestChannel(1, "nopoool", 0, []string{"k"}, nil)})
	assert.False(t, ok)
}

func TestPoolExpandMembersSkipsDisabledKeys(t *testing.T) {
	multiKey := poolTestChannel(7, "t", 0, []string{"k0", "k1", "k2"}, map[int]int{1: common.ChannelStatusAutoDisabled})
	members := ExpandPoolMembers(multiKey)
	assert.Equal(t, []MemberId{{ChannelId: 7, KeyIndex: 0}, {ChannelId: 7, KeyIndex: 2}}, members)

	singleKey := poolTestChannel(8, "t", 0, []string{"only"}, nil)
	assert.Equal(t, []MemberId{{ChannelId: 8, KeyIndex: -1}}, ExpandPoolMembers(singleKey))
}

func TestSelectChannelKeyForRequestConsumesPreselect(t *testing.T) {
	channel := poolTestChannel(9, "t", 0, []string{"k0", "k1"}, nil)

	c, _ := gin.CreateTestContext(nil)
	common.SetContextKey(c, constant.ContextKeyPoolSelectedKeyIndex, 1)
	key, index, err := SelectChannelKeyForRequest(c, channel)
	require.Nil(t, err)
	assert.Equal(t, "k1", key)
	assert.Equal(t, 1, index)

	// The preselect is consumed: the next call falls back to normal key
	// selection and must not return the preselected index again.
	value, exists := common.GetContextKey(c, constant.ContextKeyPoolSelectedKeyIndex)
	require.True(t, exists)
	assert.Nil(t, value)
}

func TestSelectChannelKeyForRequestSkipsRestingKeys(t *testing.T) {
	tag := "pool-test-keyrest"
	setupPoolSetting(t, pool_setting.PoolConfig{Tag: tag, Strategy: pool_setting.StrategyOrdered})
	channel := poolTestChannel(10, tag, 0, []string{"k0", "k1"}, nil)

	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 10, KeyIndex: 0}, time.Now().Add(time.Minute), false)
	key, index, err := SelectChannelKeyForRequest(poolTestContext(), channel)
	require.Nil(t, err)
	assert.Equal(t, "k1", key)
	assert.Equal(t, 1, index)

	// When every key rests the exclusion is ignored instead of failing closed.
	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 10, KeyIndex: 1}, time.Now().Add(time.Minute), false)
	key, index, err = SelectChannelKeyForRequest(poolTestContext(), channel)
	require.Nil(t, err)
	assert.Contains(t, []string{"k0", "k1"}, key)
}

func TestPoolChannelUnavailableRequiresAllMembersResting(t *testing.T) {
	tag := "pool-test-unavail"
	setupPoolSetting(t, pool_setting.PoolConfig{Tag: tag, Strategy: pool_setting.StrategyOrdered})
	channel := poolTestChannel(11, tag, 0, []string{"k0", "k1"}, nil)

	assert.False(t, PoolChannelUnavailable(channel))
	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 11, KeyIndex: 0}, time.Now().Add(time.Minute), false)
	assert.False(t, PoolChannelUnavailable(channel))
	poolRuntimeFor(tag).markResting(MemberId{ChannelId: 11, KeyIndex: 1}, time.Now().Add(time.Minute), false)
	assert.True(t, PoolChannelUnavailable(channel))
}

func TestPoolMemberString(t *testing.T) {
	assert.Equal(t, "12", MemberId{ChannelId: 12, KeyIndex: -1}.String())
	assert.Equal(t, "12:3", MemberId{ChannelId: 12, KeyIndex: 3}.String())
}

func poolTestContext() *gin.Context {
	c, _ := gin.CreateTestContext(nil)
	return c
}
