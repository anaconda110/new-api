package service

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/pool_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// strictBindingFixture wires a strict session-affinity rule bound to one
// multi-key channel (id 555, two keys) placed in a pool, so the selection-time
// strict branch in SelectChannelForRequest can be exercised for both the
// partial-resting and total-resting sub-cases.
type strictBindingFixture struct {
	channelID      int
	tag            string
	affinityValue  string
	cacheKeySuffix string
}

func setupStrictBindingFixture(t *testing.T, keys []string) *strictBindingFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)

	const (
		channelID = 555
		tag       = "strictpool"
	)

	// Memory cache backs CacheGetChannel + IsChannelEnabledForGroupModel; build
	// it from the seeded channel+ability so the affinity path never touches the
	// abilities SQL (which needs model.initCol()'s dialect column quoting).
	originalDB := model.DB
	originalMemCache := common.MemoryCacheEnabled
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB = db
	common.MemoryCacheEnabled = true

	ch := &model.Channel{
		Id: channelID, Name: "strict-ch", Type: 1,
		Status: common.ChannelStatusEnabled, Key: strings.Join(keys, "\n"),
		Group: "default", Models: "gpt-test",
	}
	ch.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeySize: len(keys)}
	ch.SetTag(tag)
	require.NoError(t, db.Create(ch).Error)
	// The strict rule resolves the bound channel for group "default" + "gpt-test".
	require.NoError(t, db.Create(&model.Ability{
		Group: "default", Model: "gpt-test", ChannelId: channelID, Enabled: true,
	}).Error)
	model.InitChannelCache()

	// Pool config so the channel resolves into a pool whose members we can rest.
	poolSetting := pool_setting.GetPoolSetting()
	origPool := *poolSetting
	poolSetting.Enabled = true
	poolSetting.Pools = []pool_setting.PoolConfig{{Tag: tag, Strategy: "ordered", RateLimitPolicy: "keep", RateLimitRestSeconds: 30}}

	// Strict session-affinity rule bound (via a request header) to the channel.
	rule := operation_setting.ChannelAffinityRule{
		Name:        "strict-binding",
		ModelRegex:  []string{"^gpt-test$"},
		SessionMode: "strict",
		KeySources: []operation_setting.ChannelAffinityKeySource{
			{Type: "request_header", Key: "X-Affinity-Key"},
		},
	}
	affinitySetting := operation_setting.GetChannelAffinitySetting()
	origEnabled := affinitySetting.Enabled
	origSession := affinitySetting.SessionMode
	origRules := affinitySetting.Rules
	affinitySetting.Enabled = true
	affinitySetting.Rules = []operation_setting.ChannelAffinityRule{rule}

	affinityValue := fmt.Sprintf("strict-%d", time.Now().UnixNano())
	cacheKeySuffix := buildChannelAffinityCacheKeySuffix(rule, "gpt-test", "default", affinityValue)
	cache := getChannelAffinityCache()
	require.NoError(t, cache.SetWithTTL(cacheKeySuffix, channelID, time.Minute))

	t.Cleanup(func() {
		_, _ = cache.DeleteMany([]string{cacheKeySuffix})
		affinitySetting.Rules = origRules
		affinitySetting.Enabled = origEnabled
		affinitySetting.SessionMode = origSession
		*poolSetting = origPool
		model.DB = originalDB
		common.MemoryCacheEnabled = originalMemCache
		// Reset the pool runtime so resting state does not leak across tests.
		poolRuntimes.Delete(tag)
	})

	return &strictBindingFixture{channelID: channelID, tag: tag, affinityValue: affinityValue, cacheKeySuffix: cacheKeySuffix}
}

// restMember marks one member of the bound channel resting (or available when
// until is in the past).
func (f *strictBindingFixture) restMember(t *testing.T, keyIndex int, resting bool) {
	t.Helper()
	until := time.Time{}
	if resting {
		until = time.Now().Add(time.Hour)
	}
	poolRuntimeFor(f.tag).markRestingUntil(MemberId{ChannelId: f.channelID, KeyIndex: keyIndex}, until, false)
}

// bindingRetained reports whether the affinity cache entry (the session's
// binding) survived the selection call.
func (f *strictBindingFixture) bindingRetained(t *testing.T) bool {
	t.Helper()
	_, found, err := getChannelAffinityCache().Get(f.cacheKeySuffix)
	require.NoError(t, err)
	return found
}

func (f *strictBindingFixture) newRequest() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("X-Affinity-Key", f.affinityValue)
	return ctx, rec
}

// TestStrictSessionBindingFailsFastWhenPoolExhausted pins Tenny's ruling: a
// strict session whose bound channel has NO live member fails fast with
// strict_session_binding_unavailable (no silent reroute) while RETAINING the
// binding so the session returns to the same pool after cooldown. It also pins
// that a bound channel with a live member is still served normally (no error).
//
// The discriminator under test is "can the bound channel serve THIS request?"
// (the !affinityUsable condition), not "partial vs total resting": any bound
// channel that cannot serve — fully resting, single-key resting, or up-but-
// filtered-out for this request — fails fast with the binding retained.
func TestStrictSessionBindingFailsFastWhenPoolExhausted(t *testing.T) {
	f := setupStrictBindingFixture(t, []string{"key-a", "key-b"})
	retry := &RetryParam{TokenGroup: "default", ModelName: "gpt-test", Retry: intp(0)}

	t.Run("total resting fails fast and keeps binding", func(t *testing.T) {
		f.restMember(t, 0, true)
		f.restMember(t, 1, true)

		ctx, _ := f.newRequest()
		retry.Ctx = ctx
		channel, _, selectErr := SelectChannelForRequest(ctx, "gpt-test", retry)

		require.NotNil(t, selectErr, "total-resting bound channel must fail fast, not reroute")
		assert.Equal(t, "strict_session_binding_unavailable", selectErr.Message)
		assert.Nil(t, channel, "no channel should be served when the bound pool is exhausted")
		assert.True(t, f.bindingRetained(t),
			"binding must be retained so the strict session rebinds after cooldown")
	})

	t.Run("partial resting still serves from the live member", func(t *testing.T) {
		f.restMember(t, 0, true)
		f.restMember(t, 1, false) // key-b live

		ctx, _ := f.newRequest()
		retry.Ctx = ctx
		channel, _, selectErr := SelectChannelForRequest(ctx, "gpt-test", retry)

		require.Nil(t, selectErr, "a bound channel with a live member must be served, not error")
		require.NotNil(t, channel)
		assert.Equal(t, f.channelID, channel.Id)
	})

	t.Run("no resting serves normally", func(t *testing.T) {
		f.restMember(t, 0, false)
		f.restMember(t, 1, false)

		ctx, _ := f.newRequest()
		retry.Ctx = ctx
		channel, _, selectErr := SelectChannelForRequest(ctx, "gpt-test", retry)

		require.Nil(t, selectErr)
		require.NotNil(t, channel)
		assert.Equal(t, f.channelID, channel.Id)
	})

	// Tao's contract explicitly includes single-key channels: a single-key
	// bound channel whose only member is resting "cannot serve the request", so
	// it must fail fast (nil channel, strict error) with the binding retained.
	t.Run("single-key channel resting fails fast and keeps binding", func(t *testing.T) {
		single := setupStrictBindingFixture(t, []string{"only-key"})
		single.restMember(t, 0, true)

		ctx, _ := single.newRequest()
		retry.Ctx = ctx
		channel, _, selectErr := SelectChannelForRequest(ctx, "gpt-test", retry)

		require.NotNil(t, selectErr)
		assert.Equal(t, "strict_session_binding_unavailable", selectErr.Message)
		assert.Nil(t, channel, "no reroute to pool or group: channel must be nil")
		assert.True(t, single.bindingRetained(t))
	})

	// A member can be up but still unusable for THIS request (here: the bound
	// channel does not serve the request's group). That is also "cannot serve",
	// so strict must fail fast rather than silently reroute to another channel.
	// (The filtered-out arm clears the binding per keep_on_channel_disabled — a
	// different binding outcome than the resting arm; that matrix is pinned by
	// controller TestRequestPolicyRoutingDatabaseMatrix, so we assert only the
	// fail-fast/no-reroute discriminator here.)
	t.Run("member up but filtered out for this request fails fast", func(t *testing.T) {
		filtered := setupStrictBindingFixtureInGroup(t, []string{"key-a", "key-b"}, "premium")
		// No member resting: the channel is up, just not usable for group "default".

		ctx, _ := filtered.newRequest()
		retry.Ctx = ctx
		channel, _, selectErr := SelectChannelForRequest(ctx, "gpt-test", retry)

		require.NotNil(t, selectErr, "a bound channel that cannot serve this request must fail fast")
		assert.Equal(t, "strict_session_binding_unavailable", selectErr.Message)
		assert.Nil(t, channel, "no reroute to pool or group: channel must be nil")
	})
}

// setupStrictBindingFixtureInGroup is setupStrictBindingFixture but places the
// bound channel in a different user group, so it is up yet filtered-out for a
// "default"-group request (exercising the filtered-out arm of !affinityUsable).
func setupStrictBindingFixtureInGroup(t *testing.T, keys []string, group string) *strictBindingFixture {
	t.Helper()
	f := setupStrictBindingFixture(t, keys)
	// Re-point the seeded channel + ability to another group so the "default"
	// request cannot use it, then rebuild the cache from the updated rows.
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", f.channelID).Update("group", group).Error)
	require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", f.channelID).Update("group", group).Error)
	model.InitChannelCache()
	return f
}

func intp(i int) *int { return &i }
