package service

import (
	"math/rand"
	"sort"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/pool_setting"
	"github.com/gin-gonic/gin"
)

// poolCandidate is one schedulable member of a pool with the state the
// strategies need. The pool runtime state is copied at selection time so the
// strategies stay pure functions over a snapshot.
type poolCandidate struct {
	Member     MemberId
	Channel    *model.Channel
	Annotation pool_setting.MemberAnnotation
	Windows    []WindowSnapshot
	Penalty    int
	LatencyMs  float64
	UsedReqs   int64
}

// useBeforeExpiryHoldback is the short-window usage fraction at which the
// use-before-expiry strategy holds a member back until everything else is
// unable to respond. Weekly and monthly windows are never held back.
const useBeforeExpiryHoldback = 0.9

// trySelectPoolChannel applies pool scheduling to a priority-tier candidate
// set. It returns nil whenever pool scheduling does not own the selection —
// pool scheduling disabled, no pool for the tier's tag, mixed tags, or every
// member resting — and the caller falls back to the legacy weighted random.
func trySelectPoolChannel(c *gin.Context, group string, modelName string, priorityRetry int) *model.Channel {
	if !pool_setting.GetPoolSetting().Enabled || c == nil {
		return nil
	}
	tier, err := model.GetPriorityTierCandidates(group, modelName, priorityRetry, GetChannelConstraints(c).Filters)
	if err != nil || len(tier) == 0 {
		return nil
	}
	pool, ok := poolForTier(tier)
	if !ok {
		return nil
	}
	channel, keyIndex := selectPoolMember(pool, tier, usedChannelIds(c))
	if channel == nil && pool.ExhaustedWaitMs > 0 {
		// Every member is resting: wait briefly for the nearest cooldown to
		// expire (upstream waits ~2s in this situation) before giving the
		// legacy path a chance. The wait is bounded by the nearest rest.
		waitMs := pool.ExhaustedWaitMs
		if nearest := nearestRestMs(pool, tier); nearest > 0 && nearest < int64(waitMs) {
			waitMs = int(nearest)
		}
		logger.LogWarn(c, common.LogText("pool %s exhausted, waiting %dms for a member to free up", pool.Tag, waitMs))
		select {
		case <-time.After(time.Duration(waitMs) * time.Millisecond):
		case <-c.Request.Context().Done():
			return nil
		}
		releasePoolInflight(c)
		channel, keyIndex = selectPoolMember(pool, tier, usedChannelIds(c))
	}
	if channel == nil {
		logger.LogWarn(c, common.LogText("pool %s has no available member, falling back to weighted random", pool.Tag))
		return nil
	}
	releasePoolInflight(c)
	selected := MemberId{ChannelId: channel.Id, KeyIndex: keyIndex}
	poolRuntimeFor(pool.Tag).acquireInflight(selected)
	c.Set(poolInflightContextKey, selected)
	if keyIndex >= 0 {
		common.SetContextKey(c, constant.ContextKeyPoolSelectedKeyIndex, keyIndex)
	}
	c.Set(string(constant.ContextKeyPoolSelection), map[string]any{
		"tag":      pool.Tag,
		"strategy": pool.Strategy,
		"member":   memberString(channel.Id, keyIndex),
	})
	return channel
}

// poolForTier requires the whole priority tier to share one tag that has a
// pool configuration. Mixing tagged and untagged channels inside one tier is
// a configuration smell: scheduling them as one pool would surprise the
// untagged channels' owner, so the pool declines and legacy scheduling wins.
func poolForTier(tier []*model.Channel) (pool_setting.PoolConfig, bool) {
	tag := tier[0].GetTag()
	if tag == "" {
		return pool_setting.PoolConfig{}, false
	}
	for _, channel := range tier {
		if channel.GetTag() != tag {
			logger.LogWarn(nil, common.LogText("pool scheduling skipped: priority tier mixes tags (saw %q and %q)", tag, channel.GetTag()))
			return pool_setting.PoolConfig{}, false
		}
	}
	return pool_setting.GetPoolByTag(tag)
}

func usedChannelIds(c *gin.Context) map[int]bool {
	used := make(map[int]bool)
	for _, id := range c.GetStringSlice("use_channel") {
		if channelId, err := strconv.Atoi(id); err == nil {
			used[channelId] = true
		}
	}
	return used
}

// selectPoolMember expands the tier channels into members and dispatches to
// the configured strategy. It returns the chosen channel and its key index
// (-1 for single-key channels).
func selectPoolMember(pool pool_setting.PoolConfig, tier []*model.Channel, used map[int]bool) (*model.Channel, int) {
	candidates := buildPoolCandidates(pool, tier)
	available := filterAvailableMembersCapped(pool, candidates, used, pool.MaxConcurrency)
	if len(available) == 0 {
		return nil, -1
	}
	strategy := pool.Strategy
	if strategy == "" {
		strategy = pool_setting.StrategySmart
	}
	selected := dispatchPoolStrategy(strategy, pool, available, used)
	if selected == nil {
		return nil, -1
	}
	return selected.Channel, selected.Member.KeyIndex
}

// nearestRestMs returns the milliseconds until the earliest resting member
// frees up, or 0 when nothing rests.
func nearestRestMs(pool pool_setting.PoolConfig, tier []*model.Channel) int64 {
	rt := poolRuntimeFor(pool.Tag)
	now := time.Now()
	var nearest int64 = -1
	for _, channel := range tier {
		for _, member := range ExpandPoolMembers(channel) {
			d := rt.restingUntil(member).Sub(now).Milliseconds()
			if d > 0 && (nearest < 0 || d < nearest) {
				nearest = d
			}
		}
	}
	return nearest
}

func buildPoolCandidates(pool pool_setting.PoolConfig, tier []*model.Channel) []poolCandidate {
	candidates := make([]poolCandidate, 0, len(tier)*2)
	for _, channel := range tier {
		if channel == nil || channel.Status != common.ChannelStatusEnabled {
			continue
		}
		rt := poolRuntimeFor(pool.Tag)
		for _, member := range ExpandPoolMembers(channel) {
			state := rt.memberRuntimeOf(member)
			annotation := pool.AnnotationFor(channel.Id, member.KeyIndex)
			windows := state.windows
			if len(windows) == 0 && len(annotation.ManualWindows) > 0 {
				// No collector snapshot yet: the administrator's manual
				// windows keep quota-aware strategies working. Expiry hours
				// are relative to now, so stale configs self-correct.
				windows = manualWindowsToSnapshots(annotation.ManualWindows)
			}
			candidates = append(candidates, poolCandidate{
				Member:     member,
				Channel:    channel,
				Annotation: annotation,
				Windows:    windows,
				Penalty:    state.penalty,
				LatencyMs:  state.latencyMs,
				UsedReqs:   state.usedRequests,
			})
		}
	}
	return candidates
}

func manualWindowsToSnapshots(manual []pool_setting.ManualWindow) []WindowSnapshot {
	snapshots := make([]WindowSnapshot, 0, len(manual))
	now := time.Now()
	for _, window := range manual {
		snapshot := WindowSnapshot{
			Remaining:   window.Remaining,
			Capacity:    window.Capacity,
			ShortWindow: window.ShortWindow,
			Source:      "manual",
		}
		if window.ExpiryHours > 0 {
			snapshot.ExpiryAt = now.Add(time.Duration(window.ExpiryHours * float64(time.Hour)))
		}
		snapshots = append(snapshots, snapshot)
	}
	return snapshots
}

// filterAvailableMembers drops members that are resting or whose channel was
// already tried in this request. Resting members only drop out while at least
// one healthy member remains, mirroring the model-level exclusion fallback.
func filterAvailableMembers(pool pool_setting.PoolConfig, candidates []poolCandidate, used map[int]bool) []poolCandidate {
	return filterAvailableMembersCapped(pool, candidates, used, pool.MaxConcurrency)
}

func filterAvailableMembersCapped(pool pool_setting.PoolConfig, candidates []poolCandidate, used map[int]bool, maxConcurrency int) []poolCandidate {
	available := make([]poolCandidate, 0, len(candidates))
	now := time.Now()
	rt := poolRuntimeFor(pool.Tag)
	for _, candidate := range candidates {
		if used[candidate.Channel.Id] {
			continue
		}
		if rt.restingUntil(candidate.Member).After(now) {
			continue
		}
		if maxConcurrency > 0 && rt.inflightOf(candidate.Member) >= maxConcurrency {
			continue
		}
		available = append(available, candidate)
	}
	if len(available) == 0 {
		return nil
	}
	return available
}

func dispatchPoolStrategy(strategy string, pool pool_setting.PoolConfig, candidates []poolCandidate, used map[int]bool) *poolCandidate {
	switch strategy {
	case pool_setting.StrategyOrdered:
		return selectOrdered(candidates)
	case pool_setting.StrategyExhaustThenSwitch:
		return selectExhaustThenSwitch(pool, candidates, used)
	case pool_setting.StrategyRoundRobin:
		return selectRoundRobin(pool, candidates)
	case pool_setting.StrategyLeastUsed:
		return selectLeastUsed(candidates)
	case pool_setting.StrategyUseBeforeExpiry:
		return selectUseBeforeExpiry(candidates)
	case pool_setting.StrategyAuthWeight:
		return selectAuthWeight(candidates, 0, len(candidates))
	case pool_setting.StrategyFreeFirst:
		return selectFreeFirst(candidates)
	case pool_setting.StrategyFastest:
		return selectFastest(candidates)
	case pool_setting.StrategySmart:
		return selectSmart(candidates)
	default:
		return selectSmart(candidates)
	}
}

// orderedCandidates is the deterministic base order: channel id, then key
// index. Penalized members sink to the back, preserving their relative order,
// which is the move_last policy for order-based strategies.
func orderedCandidates(candidates []poolCandidate) []poolCandidate {
	ordered := make([]poolCandidate, len(candidates))
	copy(ordered, candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Member.ChannelId != ordered[j].Member.ChannelId {
			return ordered[i].Member.ChannelId < ordered[j].Member.ChannelId
		}
		return ordered[i].Member.KeyIndex < ordered[j].Member.KeyIndex
	})
	healthy := make([]poolCandidate, 0, len(ordered))
	penalized := make([]poolCandidate, 0, len(ordered))
	for _, candidate := range ordered {
		if candidate.Penalty > 0 {
			penalized = append(penalized, candidate)
		} else {
			healthy = append(healthy, candidate)
		}
	}
	return append(healthy, penalized...)
}

func selectOrdered(candidates []poolCandidate) *poolCandidate {
	ordered := orderedCandidates(candidates)
	return &ordered[0]
}

// selectExhaustThenSwitch keeps returning the same member until it fails or
// rests (the filter already removed resting members), then moves on in the
// deterministic order.
func selectExhaustThenSwitch(pool pool_setting.PoolConfig, candidates []poolCandidate, used map[int]bool) *poolCandidate {
	ordered := orderedCandidates(candidates)
	rt := poolRuntimeFor(pool.Tag)
	if sticky, ok := rt.stickyMember(); ok {
		for i := range ordered {
			if ordered[i].Member == sticky && !used[ordered[i].Channel.Id] {
				return &ordered[i]
			}
		}
	}
	selected := &ordered[0]
	rt.setSticky(selected.Member)
	return selected
}

func selectRoundRobin(pool pool_setting.PoolConfig, candidates []poolCandidate) *poolCandidate {
	ordered := orderedCandidates(candidates)
	start := poolRuntimeFor(pool.Tag).roundRobinNext(len(ordered))
	return &ordered[start]
}

// memberUsedFraction computes the used fraction of the member's fullest
// window, or -1 when no window has a capacity to compare against.
func memberUsedFraction(candidate poolCandidate) float64 {
	worst := -1.0
	for _, window := range candidate.Windows {
		if window.Capacity <= 0 {
			continue
		}
		used := 1 - window.Remaining/window.Capacity
		if used > worst {
			worst = used
		}
	}
	return worst
}

func selectLeastUsed(candidates []poolCandidate) *poolCandidate {
	best := 0
	for i := range candidates {
		if candidateLessUsed(candidates[i], candidates[best]) {
			best = i
		}
	}
	ties := make([]*poolCandidate, 0, len(candidates))
	for i := range candidates {
		if !candidateLessUsed(candidates[best], candidates[i]) {
			ties = append(ties, &candidates[i])
		}
	}
	return ties[rand.Intn(len(ties))]
}

func candidateLessUsed(a, b poolCandidate) bool {
	aUsed, bUsed := memberUsedFraction(a), memberUsedFraction(b)
	if aUsed >= 0 && bUsed >= 0 && aUsed != bUsed {
		return aUsed < bUsed
	}
	if aUsed < 0 && bUsed >= 0 {
		return false
	}
	if bUsed < 0 && aUsed >= 0 {
		return true
	}
	if a.UsedReqs != b.UsedReqs {
		return a.UsedReqs < b.UsedReqs
	}
	return false
}

// selectUseBeforeExpiry scores remaining ÷ hours-to-expiry and picks the
// highest, so quota that is about to void is spent first. Members whose short
// window (5h-style plans) reached the holdback usage fraction are parked
// until no other member can respond; weekly and monthly windows are not.
func selectUseBeforeExpiry(candidates []poolCandidate) *poolCandidate {
	now := time.Now()
	score := func(candidate poolCandidate) float64 {
		total := 0.0
		for _, window := range candidate.Windows {
			hours := 1.0
			if !window.ExpiryAt.IsZero() {
				hours = window.ExpiryAt.Sub(now).Hours()
				if hours <= 0 {
					continue
				}
				if hours < 0.25 {
					hours = 0.25
				}
			}
			total += window.Remaining / hours
		}
		return total
	}
	phase1 := make([]poolCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if memberShortWindowHeld(candidate, now) {
			continue
		}
		phase1 = append(phase1, candidate)
	}
	pool := phase1
	if len(pool) == 0 {
		pool = candidates
	}
	if !anyCandidateHasWindows(pool) {
		return selectLeastUsed(pool)
	}
	best := 0
	for i := range pool {
		if score(pool[i]) > score(pool[best]) {
			best = i
		}
	}
	ties := make([]*poolCandidate, 0, len(pool))
	for i := range pool {
		if score(pool[best]) == score(pool[i]) {
			ties = append(ties, &pool[i])
		}
	}
	return ties[rand.Intn(len(ties))]
}

func memberShortWindowHeld(candidate poolCandidate, now time.Time) bool {
	for _, window := range candidate.Windows {
		if !window.ShortWindow || window.Capacity <= 0 {
			continue
		}
		if 1-window.Remaining/window.Capacity >= useBeforeExpiryHoldback {
			return true
		}
	}
	return false
}

func anyCandidateHasWindows(candidates []poolCandidate) bool {
	for _, candidate := range candidates {
		if len(candidate.Windows) > 0 {
			return true
		}
	}
	return false
}

// selectAuthWeight is weighted random over the annotation weight falling back
// to the channel weight; members without any weight share the uniform
// smoothing used by the legacy scheduler.
func selectAuthWeight(candidates []poolCandidate, from, to int) *poolCandidate {
	weights := make([]int, to-from)
	sum := 0
	for i := range weights {
		candidate := candidates[from+i]
		weight := int(candidate.Annotation.AuthWeight)
		if weight == 0 {
			weight = int(candidate.Channel.GetWeight())
		}
		if weight < 0 {
			weight = 0
		}
		weights[i] = weight
		sum += weight
	}
	if sum == 0 {
		return &candidates[from+rand.Intn(to-from)]
	}
	randomWeight := rand.Intn(sum)
	for i := range weights {
		randomWeight -= weights[i]
		if randomWeight < 0 {
			return &candidates[from+i]
		}
	}
	return &candidates[from]
}

// selectFreeFirst partitions free-annotated members ahead of the rest and
// picks inside each partition by auth weight. With no free annotation the
// whole candidate set is one partition.
func selectFreeFirst(candidates []poolCandidate) *poolCandidate {
	hasFree := false
	for _, candidate := range candidates {
		if candidate.Annotation.Free {
			hasFree = true
			break
		}
	}
	if !hasFree {
		return selectAuthWeight(candidates, 0, len(candidates))
	}
	free := make([]poolCandidate, 0, len(candidates))
	paid := make([]poolCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.Annotation.Free {
			free = append(free, candidate)
		} else {
			paid = append(paid, candidate)
		}
	}
	if len(free) > 0 {
		return selectAuthWeight(free, 0, len(free))
	}
	return selectAuthWeight(paid, 0, len(paid))
}

// selectFastest prefers the lowest observed latency, falling back to the
// channel test's response time and finally to uniform random among ties.
func selectFastest(candidates []poolCandidate) *poolCandidate {
	latency := func(candidate poolCandidate) float64 {
		if candidate.LatencyMs > 0 {
			return candidate.LatencyMs
		}
		if candidate.Channel != nil && candidate.Channel.ResponseTime > 0 {
			return float64(candidate.Channel.ResponseTime)
		}
		return 0
	}
	best := 0
	for i := range candidates {
		li, lb := latency(candidates[i]), latency(candidates[best])
		if lb == 0 && li > 0 {
			best = i
			continue
		}
		if li > 0 && lb > 0 && li < lb {
			best = i
		}
	}
	ties := make([]*poolCandidate, 0, len(candidates))
	for i := range candidates {
		if latency(candidates[i]) == latency(candidates[best]) {
			ties = append(ties, &candidates[i])
		}
	}
	return ties[rand.Intn(len(ties))]
}

// selectSmart is the composite default: window headroom, observed speed and
// weight vote with fixed factors, and penalties drag the score down.
func selectSmart(candidates []poolCandidate) *poolCandidate {
	maxRemaining := 0.0
	for _, candidate := range candidates {
		for _, window := range candidate.Windows {
			if window.Remaining > maxRemaining {
				maxRemaining = window.Remaining
			}
		}
	}
	maxWeight := 1
	for _, candidate := range candidates {
		weight := int(candidate.Annotation.AuthWeight)
		if weight == 0 {
			weight = int(candidate.Channel.GetWeight())
		}
		if weight > maxWeight {
			maxWeight = weight
		}
	}
	score := func(candidate poolCandidate) float64 {
		remaining := 0.5
		if maxRemaining > 0 && len(candidate.Windows) > 0 {
			best := 0.0
			for _, window := range candidate.Windows {
				if window.Remaining/maxRemaining > best {
					best = window.Remaining / maxRemaining
				}
			}
			remaining = best
		}
		speed := 0.5
		if candidate.LatencyMs > 0 {
			speed = 1 / (1 + candidate.LatencyMs/1000)
		} else if candidate.Channel != nil && candidate.Channel.ResponseTime > 0 {
			speed = 1 / (1 + float64(candidate.Channel.ResponseTime)/1000)
		}
		weight := int(candidate.Annotation.AuthWeight)
		if weight == 0 {
			weight = int(candidate.Channel.GetWeight())
		}
		weightScore := 0.5
		if maxWeight > 0 && weight > 0 {
			weightScore = float64(weight) / float64(maxWeight)
		}
		penalty := candidate.Penalty
		if penalty > 2 {
			penalty = 2
		}
		return 0.4*remaining + 0.3*speed + 0.2*weightScore - 0.1*float64(penalty)/2
	}
	best := 0
	for i := range candidates {
		if score(candidates[i]) > score(candidates[best]) {
			best = i
		}
	}
	ties := make([]*poolCandidate, 0, len(candidates))
	for i := range candidates {
		if score(candidates[i]) == score(candidates[best]) {
			ties = append(ties, &candidates[i])
		}
	}
	return ties[rand.Intn(len(ties))]
}
