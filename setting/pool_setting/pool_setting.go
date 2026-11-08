package pool_setting

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/config"
)

// Strategy names for channel-pool member selection. A pool is a set of
// same-tag channels (plus the individual keys of multi-key channels) that are
// scheduled as one account pool.
const (
	StrategySmart             = "smart"               // availability × window pressure × latency × weight
	StrategyOrdered           = "ordered"             // deterministic order, first healthy member wins
	StrategyExhaustThenSwitch = "exhaust_then_switch" // stay on one member until it fails or rests
	StrategyRoundRobin        = "round_robin"         // strict rotation across members
	StrategyLeastUsed         = "least_used"          // lowest window usage / observation counter first
	StrategyUseBeforeExpiry   = "use_before_expiry"   // remaining ÷ hours-to-expiry, highest first
	StrategyAuthWeight        = "auth_weight"         // weighted random by channel weight / annotation
	StrategyFreeFirst         = "free_first"          // annotated-free members before paid ones
	StrategyFastest           = "fastest"             // lowest observed latency first
)

// RateLimitPolicy decides what happens to a member after an upstream 429:
// it always rests for RateLimitRestSeconds, then either returns to its
// original position (keep) or is demoted to the back of its strategy order
// (move_last).
const (
	RateLimitPolicyKeep     = "keep"
	RateLimitPolicyMoveLast = "move_last"
)

// ManualWindow describes a static quota window configured by the
// administrator for members without a live quota collector. The units of
// Remaining are whatever the administrator uses (credits, USD, requests);
// strategies only compare them relative to each other.
type ManualWindow struct {
	Remaining   float64 `json:"remaining"`              // remaining quota in the window
	Capacity    float64 `json:"capacity,omitempty"`     // window capacity; enables used-percentage rules
	ExpiryHours float64 `json:"expiry_hours"`           // hours until the window voids; 0 = no expiry concept
	ShortWindow bool    `json:"short_window,omitempty"` // short windows (e.g. 5h plans) are held back at the use-before-expiry threshold
}

// MemberAnnotation carries per-member hints. The map key is a channel id
// ("12") applying to every key of that channel, or "12:3" applying to key
// index 3 of channel 12 only; the key-level entry overrides the channel-level
// one.
type MemberAnnotation struct {
	Free          bool           `json:"free,omitempty"`
	AuthWeight    uint           `json:"auth_weight,omitempty"`
	ManualWindows []ManualWindow `json:"manual_windows,omitempty"`
}

type PoolConfig struct {
	Tag                  string                      `json:"tag"`
	Strategy             string                      `json:"strategy"`
	RateLimitPolicy      string                      `json:"rate_limit_policy"`
	RateLimitRestSeconds int                         `json:"rate_limit_rest_seconds"`
	MemberAnnotations    map[string]MemberAnnotation `json:"member_annotations,omitempty"`
}

type PoolSetting struct {
	Enabled bool `json:"enabled"`
	// QuotaRefreshMinutes is the cadence of the pool quota collection system
	// task; 0 falls back to 10 minutes.
	QuotaRefreshMinutes int          `json:"quota_refresh_minutes"`
	Pools               []PoolConfig `json:"pools"`
}

var poolSetting = PoolSetting{
	Enabled:             false,
	QuotaRefreshMinutes: 10,
	Pools:               []PoolConfig{},
}

func init() {
	config.GlobalConfig.Register("pool_setting", &poolSetting)
}

func GetPoolSetting() *PoolSetting {
	return &poolSetting
}

// GetPoolByTag returns the pool configured for the given channel tag.
func GetPoolByTag(tag string) (PoolConfig, bool) {
	if tag == "" {
		return PoolConfig{}, false
	}
	for _, pool := range poolSetting.Pools {
		if pool.Tag == tag {
			return pool, true
		}
	}
	return PoolConfig{}, false
}

// AnnotationFor resolves the annotation for a channel (keyIndex < 0 or a
// single-key channel) or one of its keys, with key-level entries overriding
// channel-level ones.
func (p PoolConfig) AnnotationFor(channelId int, keyIndex int) MemberAnnotation {
	channelKey := strconv.Itoa(channelId)
	memberKey := channelKey
	if keyIndex >= 0 {
		memberKey = fmt.Sprintf("%s:%d", channelKey, keyIndex)
	}
	annotation := MemberAnnotation{}
	if p.MemberAnnotations != nil {
		if channelLevel, ok := p.MemberAnnotations[channelKey]; ok {
			annotation = channelLevel
		}
		if keyIndex >= 0 {
			if keyLevel, ok := p.MemberAnnotations[memberKey]; ok {
				annotation = keyLevel
			}
		}
	}
	return annotation
}

// ValidatePoolOption rejects invalid pool_setting updates before they persist.
func ValidatePoolOption(key, value string) error {
	if key != "pool_setting.pools" && key != "pool_setting.enabled" {
		return nil
	}
	if key == "pool_setting.enabled" {
		return nil
	}
	var pools []PoolConfig
	if err := common.UnmarshalJsonStr(value, &pools); err != nil {
		return fmt.Errorf("pool_setting.pools must be a JSON array: %v", err)
	}
	seenTags := make(map[string]bool, len(pools))
	for i, pool := range pools {
		if strings.TrimSpace(pool.Tag) == "" {
			return fmt.Errorf("pool_setting.pools[%d].tag must not be empty", i)
		}
		if seenTags[pool.Tag] {
			return fmt.Errorf("pool_setting.pools[%d].tag %q is duplicated", i, pool.Tag)
		}
		seenTags[pool.Tag] = true
		if !validStrategy(pool.Strategy) {
			return fmt.Errorf("pool_setting.pools[%d].strategy %q is not a supported strategy", i, pool.Strategy)
		}
		if pool.RateLimitPolicy != "" && pool.RateLimitPolicy != RateLimitPolicyKeep && pool.RateLimitPolicy != RateLimitPolicyMoveLast {
			return fmt.Errorf("pool_setting.pools[%d].rate_limit_policy %q must be %q or %q", i, pool.RateLimitPolicy, RateLimitPolicyKeep, RateLimitPolicyMoveLast)
		}
		if pool.RateLimitRestSeconds < 0 {
			return fmt.Errorf("pool_setting.pools[%d].rate_limit_rest_seconds must not be negative", i)
		}
	}
	return nil
}

func validStrategy(strategy string) bool {
	switch strategy {
	case StrategySmart, StrategyOrdered, StrategyExhaustThenSwitch, StrategyRoundRobin,
		StrategyLeastUsed, StrategyUseBeforeExpiry, StrategyAuthWeight, StrategyFreeFirst, StrategyFastest:
		return true
	}
	return false
}
