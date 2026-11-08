package service

import (
	"fmt"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/pool_setting"
	"github.com/gin-gonic/gin"
)

// MemberId identifies one scheduling member of a channel pool. A single-key
// channel is one member with KeyIndex -1; a multi-key channel contributes one
// member per enabled key, addressed by key index.
type MemberId struct {
	ChannelId int `json:"channel_id"`
	KeyIndex  int `json:"key_index"`
}

func (m MemberId) String() string {
	if m.KeyIndex < 0 {
		return fmt.Sprintf("%d", m.ChannelId)
	}
	return fmt.Sprintf("%d:%d", m.ChannelId, m.KeyIndex)
}

func memberString(channelId int, keyIndex int) string {
	return MemberId{ChannelId: channelId, KeyIndex: keyIndex}.String()
}

// WindowSnapshot is one quota window of a member, fed by a quota collector or
// by the administrator's manual configuration. Remaining uses the collector's
// units; only comparisons between members of the same pool are meaningful.
type WindowSnapshot struct {
	Remaining   float64   `json:"remaining"`
	Capacity    float64   `json:"capacity,omitempty"`
	ExpiryAt    time.Time `json:"expiry_at,omitempty"`
	ShortWindow bool      `json:"short_window,omitempty"`
	Source      string    `json:"source,omitempty"` // manual | collector
}

type memberRuntime struct {
	restingUntil time.Time
	penalty      int
	failures     int
	inflight     int
	latencyMs    float64
	latencyN     int64
	usedRequests int64
	windows      []WindowSnapshot
	quotaUnknown bool
}

func (m *memberRuntime) observeSuccess(latencyMs int64) {
	m.usedRequests++
	if latencyMs <= 0 {
		return
	}
	// Exponentially weighted moving average, alpha = 1/4.
	if m.latencyN == 0 {
		m.latencyMs = float64(latencyMs)
	} else {
		m.latencyMs = m.latencyMs*0.75 + float64(latencyMs)*0.25
	}
	m.latencyN++
}

type poolRuntime struct {
	mu        sync.Mutex
	members   map[string]*memberRuntime
	rrIndex   int
	sticky    MemberId
	stickySet bool
}

var poolRuntimes sync.Map // pool tag -> *poolRuntime

func poolRuntimeFor(tag string) *poolRuntime {
	if value, ok := poolRuntimes.Load(tag); ok {
		return value.(*poolRuntime)
	}
	rt := &poolRuntime{members: make(map[string]*memberRuntime)}
	actual, _ := poolRuntimes.LoadOrStore(tag, rt)
	return actual.(*poolRuntime)
}

func (p *poolRuntime) member(id MemberId) *memberRuntime {
	key := id.String()
	if m, ok := p.members[key]; ok {
		return m
	}
	m := &memberRuntime{}
	p.members[key] = m
	return m
}

// markResting puts a member to rest until the given time and applies the
// pool's rate-limit policy. backoff escalates the duration on repeated
// failures: base << min(failures-1, 3), capped at 8x.
func (p *poolRuntime) markResting(id MemberId, base time.Duration, until time.Time, moveLast bool, backoff bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.member(id)
	m.failures++
	rest := base
	if backoff && m.failures > 1 {
		shift := m.failures - 1
		if shift > 3 {
			shift = 3
		}
		rest = base << shift
	}
	m.restingUntil = time.Now().Add(rest)
	if moveLast {
		m.penalty++
	}
	if p.stickySet && p.sticky == id {
		p.stickySet = false
	}
}

func (p *poolRuntime) restingUntil(id MemberId) time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.member(id).restingUntil
}

func (p *poolRuntime) bumpPenalty(id MemberId) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.member(id).penalty++
}

func (p *poolRuntime) observeSuccess(id MemberId, latencyMs int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.member(id)
	m.observeSuccess(latencyMs)
	m.failures = 0
}

// acquireInflight bumps a member's in-flight counter and reports the new
// value; releaseInflight drops it. Both are request-lifecycle scoped.
func (p *poolRuntime) acquireInflight(id MemberId) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.member(id)
	m.inflight++
	return m.inflight
}

func (p *poolRuntime) releaseInflight(id MemberId) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.members[id.String()]; ok && m.inflight > 0 {
		m.inflight--
	}
}

func (p *poolRuntime) inflightOf(id MemberId) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.member(id).inflight
}

func (p *poolRuntime) setWindows(id MemberId, windows []WindowSnapshot, quotaUnknown bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.member(id)
	m.windows = windows
	m.quotaUnknown = quotaUnknown
}

// memberRuntimeOf returns a copy of the member's runtime state.
func (p *poolRuntime) memberRuntimeOf(id MemberId) memberRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := *p.member(id)
	return state
}

func (p *poolRuntime) memberStates() map[string]memberRuntime {
	p.mu.Lock()
	defer p.mu.Unlock()
	states := make(map[string]memberRuntime, len(p.members))
	for key, m := range p.members {
		states[key] = *m
	}
	return states
}

// MemberRuntimeStatus is the exported view of one member's runtime state for
// the management API and the channel console.
type MemberRuntimeStatus struct {
	RestingUntil time.Time        `json:"resting_until"`
	Penalty      int              `json:"penalty"`
	Failures     int              `json:"failures"`
	Inflight     int              `json:"inflight"`
	LatencyMs    float64          `json:"latency_ms"`
	UsedRequests int64            `json:"used_requests"`
	Windows      []WindowSnapshot `json:"windows"`
	QuotaUnknown bool             `json:"quota_unknown"`
}

func (p *poolRuntime) memberStatuses() map[string]MemberRuntimeStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	statuses := make(map[string]MemberRuntimeStatus, len(p.members))
	for key, m := range p.members {
		statuses[key] = MemberRuntimeStatus{
			RestingUntil: m.restingUntil,
			Penalty:      m.penalty,
			Failures:     m.failures,
			Inflight:     m.inflight,
			LatencyMs:    m.latencyMs,
			UsedRequests: m.usedRequests,
			Windows:      m.windows,
			QuotaUnknown: m.quotaUnknown,
		}
	}
	return statuses
}

// roundRobinNext returns the ordered index to start scanning from and
// advances the cursor past it.
func (p *poolRuntime) roundRobinNext(length int) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if length <= 0 {
		return 0
	}
	idx := p.rrIndex % length
	p.rrIndex = (idx + 1) % length
	return idx
}

func (p *poolRuntime) stickyMember() (MemberId, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sticky, p.stickySet
}

func (p *poolRuntime) setSticky(id MemberId) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sticky = id
	p.stickySet = true
}

// PoolTagForChannel returns the pool configuration matching the channel's
// tag, if pool scheduling is enabled.
func PoolTagForChannel(channel *model.Channel) (pool_setting.PoolConfig, bool) {
	if channel == nil || !pool_setting.GetPoolSetting().Enabled {
		return pool_setting.PoolConfig{}, false
	}
	return pool_setting.GetPoolByTag(channel.GetTag())
}

// ExpandPoolMembers lists the scheduling members of a channel: the enabled
// key indexes for a multi-key channel, or the single member with KeyIndex -1.
func ExpandPoolMembers(channel *model.Channel) []MemberId {
	if channel == nil {
		return nil
	}
	if !channel.ChannelInfo.IsMultiKey {
		return []MemberId{{ChannelId: channel.Id, KeyIndex: -1}}
	}
	keys := channel.GetKeys()
	members := make([]MemberId, 0, len(keys))
	for i := range keys {
		if channelKeyEnabled(channel, i) {
			members = append(members, MemberId{ChannelId: channel.Id, KeyIndex: i})
		}
	}
	return members
}

func channelKeyEnabled(channel *model.Channel, index int) bool {
	status, ok := channel.ChannelInfo.MultiKeyStatusList[index]
	if !ok {
		return true
	}
	return status == common.ChannelStatusEnabled
}

func channelMemberResting(channel *model.Channel, member MemberId) bool {
	pool, ok := PoolTagForChannel(channel)
	if !ok {
		return false
	}
	return poolRuntimeFor(pool.Tag).restingUntil(member).After(time.Now())
}

// PoolChannelUnavailable reports whether every member of the channel's pool
// is currently resting. It lets session affinity treat the bound channel as
// unavailable so strict sessions fail fast and prefer sessions reroute.
func PoolChannelUnavailable(channel *model.Channel) bool {
	if channel == nil {
		return false
	}
	members := ExpandPoolMembers(channel)
	if len(members) == 0 {
		return false
	}
	for _, member := range members {
		if !channelMemberResting(channel, member) {
			return false
		}
	}
	return true
}

// currentAttemptMember resolves the member that served the current request
// attempt from the context written by SetupContextForSelectedChannel.
func currentAttemptMember(c *gin.Context, channelId int) MemberId {
	if c != nil && common.GetContextKeyBool(c, constant.ContextKeyChannelIsMultiKey) {
		return MemberId{ChannelId: channelId, KeyIndex: c.GetInt(string(constant.ContextKeyChannelMultiKeyIndex))}
	}
	return MemberId{ChannelId: channelId, KeyIndex: -1}
}

// MarkPoolRateLimited records an upstream 429 for the member that served the
// current attempt: the member rests for the pool's configured duration and,
// under the move_last policy, gains a penalty. Channels whose tag has no pool
// configuration are ignored, and nothing here disables a channel or key.
func MarkPoolRateLimited(c *gin.Context, channelId int) {
	if !pool_setting.GetPoolSetting().Enabled {
		return
	}
	channel, err := model.CacheGetChannel(channelId)
	if err != nil || channel == nil {
		return
	}
	pool, ok := PoolTagForChannel(channel)
	if !ok {
		return
	}
	restSeconds := pool.RateLimitRestSeconds
	if restSeconds <= 0 {
		restSeconds = 60
	}
	member := currentAttemptMember(c, channelId)
	poolRuntimeFor(pool.Tag).markResting(
		member,
		time.Duration(restSeconds)*time.Second,
		time.Now(),
		pool.RateLimitPolicy == pool_setting.RateLimitPolicyMoveLast,
		pool.RateLimitBackoff,
	)
}

// ObservePoolMemberUsage feeds the pool's observational counters after a
// successful response. It never touches billing state; the counters only
// rank members inside least-used / smart / fastest strategies.
func ObservePoolMemberUsage(c *gin.Context, channelId int) {
	if !pool_setting.GetPoolSetting().Enabled {
		return
	}
	channel, err := model.CacheGetChannel(channelId)
	if err != nil || channel == nil {
		return
	}
	pool, ok := PoolTagForChannel(channel)
	if !ok {
		return
	}
	latencyMs := int64(0)
	start := common.GetContextKeyTime(c, constant.ContextKeyRequestStartTime)
	if !start.IsZero() {
		latencyMs = time.Since(start).Milliseconds()
	}
	member := currentAttemptMember(c, channelId)
	poolRuntimeFor(pool.Tag).observeSuccess(member, latencyMs)
}

// SelectChannelKeyForRequest picks the upstream key for the selected channel.
// It consumes the pool strategy's preselected key index when present, then
// falls back to GetNextEnabledKey while excluding keys whose pool member is
// resting. When the exclusion would remove every enabled key it is ignored,
// mirroring the model-level graceful degradation.
func SelectChannelKeyForRequest(c *gin.Context, channel *model.Channel) (string, int, *types.NewAPIError) {
	if value, exists := common.GetContextKey(c, constant.ContextKeyPoolSelectedKeyIndex); exists {
		c.Set(string(constant.ContextKeyPoolSelectedKeyIndex), nil)
		if index, ok := value.(int); ok && index >= 0 && channel.ChannelInfo.IsMultiKey {
			keys := channel.GetKeys()
			if index < len(keys) && channelKeyEnabled(channel, index) && !channelMemberResting(channel, MemberId{ChannelId: channel.Id, KeyIndex: index}) {
				return keys[index], index, nil
			}
		}
	}
	return channel.GetNextEnabledKeyExcluding(poolRestingExclusions(channel))
}

func poolRestingExclusions(channel *model.Channel) map[int]bool {
	if !channel.ChannelInfo.IsMultiKey {
		return nil
	}
	pool, ok := PoolTagForChannel(channel)
	if !ok {
		return nil
	}
	keys := channel.GetKeys()
	excluded := make(map[int]bool)
	available := 0
	for i := range keys {
		if !channelKeyEnabled(channel, i) {
			continue
		}
		if poolRuntimeFor(pool.Tag).restingUntil(MemberId{ChannelId: channel.Id, KeyIndex: i}).After(time.Now()) {
			excluded[i] = true
			continue
		}
		available++
	}
	if available == 0 {
		return nil
	}
	return excluded
}

// SetMemberWindows stores collector or manual window snapshots for a member.
// Collector failures must pass quotaUnknown=true and never disable anything:
// a broken quota read must not look like a broken account.
func SetMemberWindows(tag string, member MemberId, windows []WindowSnapshot, quotaUnknown bool) {
	poolRuntimeFor(tag).setWindows(member, windows, quotaUnknown)
}

// PoolStatusSnapshot returns the runtime state of every member of a pool for
// the management API, keyed by member string.
func PoolStatusSnapshot(tag string) map[string]MemberRuntimeStatus {
	return poolRuntimeFor(tag).memberStatuses()
}

const (
	poolInflightContextKey    = "pool_inflight_member"
	poolInflightTagContextKey = "pool_inflight_tag"
)

// releasePoolInflight drops the in-flight slot acquired for the previous
// attempt of this request, if any.
func releasePoolInflight(c *gin.Context) {
	if c == nil {
		return
	}
	if value, ok := c.Get(poolInflightContextKey); ok {
		if member, ok := value.(MemberId); ok {
			if tag := c.GetString(poolInflightTagContextKey); tag != "" {
				poolRuntimeFor(tag).releaseInflight(member)
			}
		}
	}
	c.Set(poolInflightContextKey, nil)
}

// PoolServedAttempt reports whether the current attempt was selected by the
// pool strategy layer.
func PoolServedAttempt(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := common.GetContextKey(c, constant.ContextKeyPoolSelection)
	return ok && value != nil
}

// ReleasePoolInflight releases the attempt's member slot at request end.
func ReleasePoolInflight(c *gin.Context) {
	releasePoolInflight(c)
}

// AppendPoolAdminInfo adds the pool scheduling decision of the current
// attempt to the consume log's admin info, mirroring the channel affinity
// hint.
func AppendPoolAdminInfo(c *gin.Context, other *model.LogOther) {
	if c == nil || other == nil {
		return
	}
	if info, ok := c.Get(string(constant.ContextKeyPoolSelection)); ok && info != nil {
		other.SetAdmin("pool", info)
	}
}
