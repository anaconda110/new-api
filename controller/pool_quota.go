package controller

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/pool_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

// balanceQuotaCollector reuses the channel balance adapters as the fallback
// pool quota collector: one balance-shaped window with no capacity and no
// expiry, so balance-fed pools degrade to "most remaining wins" instead of
// lying about window percentages. Multi-key members cannot be split (the
// balance APIs report one account per channel), so only the channel-level
// member is collected for them.
type balanceQuotaCollector struct{}

func (balanceQuotaCollector) Collect(channel *model.Channel, keyIndex int) ([]model.PoolMemberWindow, error) {
	if channel.ChannelInfo.IsMultiKey && keyIndex >= 0 {
		return nil, fmt.Errorf("multi-key channels do not support per-key balance collection")
	}
	result, err := updateChannelBalance(channel)
	if err != nil {
		return nil, err
	}
	return []model.PoolMemberWindow{{
		Remaining: result.Balance,
		Source:    "balance",
	}}, nil
}

func init() {
	service.RegisterDefaultQuotaCollector(balanceQuotaCollector{})
}

// poolQuotaHandler runs the periodic pool quota collection job inside the
// system task framework so a DB lease dedups execution across masters.
type poolQuotaHandler struct{}

func (poolQuotaHandler) Type() string { return model.SystemTaskTypePoolQuota }

func (poolQuotaHandler) Enabled() bool {
	setting := pool_setting.GetPoolSetting()
	return setting.Enabled && len(setting.Pools) > 0
}

func (poolQuotaHandler) Interval() time.Duration {
	minutes := pool_setting.GetPoolSetting().QuotaRefreshMinutes
	if minutes <= 0 {
		minutes = 10
	}
	return time.Duration(minutes) * time.Minute
}

func (poolQuotaHandler) NewPayload() any { return nil }

func (poolQuotaHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	collected := service.RunPoolQuotaCollection()
	finishSystemTaskHandler(task, runnerID, model.SystemTaskStatusSucceeded, map[string]any{
		"collected": collected,
	}, nil)
}

// poolMemberStatus is one member row of the pool status API.
type poolMemberStatus struct {
	ChannelId    int                           `json:"channel_id"`
	KeyIndex     int                           `json:"key_index"`
	Member       string                        `json:"member"`
	ChannelName  string                        `json:"channel_name"`
	ChannelType  int                           `json:"channel_type"`
	ChannelState int                           `json:"channel_status"`
	Collector    bool                          `json:"collector"`
	Penalty      int                           `json:"penalty"`
	Failures     int                           `json:"failures"`
	Inflight     int                           `json:"inflight"`
	RestingUntil *time.Time                    `json:"resting_until,omitempty"`
	LatencyMs    float64                       `json:"latency_ms"`
	UsedRequests int64                         `json:"used_requests"`
	QuotaUnknown bool                          `json:"quota_unknown"`
	Windows      []model.PoolMemberWindow      `json:"windows,omitempty"`
	Annotation   pool_setting.MemberAnnotation `json:"annotation"`
}

type poolStatus struct {
	Tag                  string             `json:"tag"`
	Strategy             string             `json:"strategy"`
	RateLimitPolicy      string             `json:"rate_limit_policy"`
	RateLimitRestSeconds int                `json:"rate_limit_rest_seconds"`
	Members              []poolMemberStatus `json:"members"`
}

// GetPoolStatus returns every configured pool with the live member states
// shown in the channel console.
func GetPoolStatus(c *gin.Context) {
	setting := pool_setting.GetPoolSetting()
	if !setting.Enabled || len(setting.Pools) == 0 {
		common.ApiSuccess(c, map[string]any{"pools": []poolStatus{}})
		return
	}
	pools := make([]poolStatus, 0, len(setting.Pools))
	for _, pool := range setting.Pools {
		status := poolStatus{
			Tag:                  pool.Tag,
			Strategy:             pool.Strategy,
			RateLimitPolicy:      pool.RateLimitPolicy,
			RateLimitRestSeconds: pool.RateLimitRestSeconds,
			Members:              []poolMemberStatus{},
		}
		channels, err := model.GetChannelsByTag(pool.Tag, false, true)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		states := service.PoolStatusSnapshot(pool.Tag)
		for _, channel := range channels {
			for _, member := range service.ExpandPoolMembers(channel) {
				state := states[member.String()]
				memberStatus := poolMemberStatus{
					ChannelId:    channel.Id,
					KeyIndex:     member.KeyIndex,
					Member:       member.String(),
					ChannelName:  channel.Name,
					ChannelType:  channel.Type,
					ChannelState: channel.Status,
					Collector:    service.HasQuotaCollector(channel.Type),
					Penalty:      state.Penalty,
					Failures:     state.Failures,
					Inflight:     state.Inflight,
					LatencyMs:    state.LatencyMs,
					UsedRequests: state.UsedRequests,
					QuotaUnknown: state.QuotaUnknown,
					Annotation:   pool.AnnotationFor(channel.Id, member.KeyIndex),
				}
				if state.RestingUntil.After(time.Now()) {
					resting := state.RestingUntil
					memberStatus.RestingUntil = &resting
				}
				for _, window := range state.Windows {
					memberStatus.Windows = append(memberStatus.Windows, model.PoolMemberWindow{
						Remaining:   window.Remaining,
						Capacity:    window.Capacity,
						ExpiryAt:    window.ExpiryAt,
						ShortWindow: window.ShortWindow,
						Source:      window.Source,
					})
				}
				status.Members = append(status.Members, memberStatus)
			}
		}
		pools = append(pools, status)
	}
	common.ApiSuccess(c, map[string]any{"pools": pools})
}

// PoolWhy explains why a (group, model) request would or would not be served
// by each pool member right now.
func PoolWhy(c *gin.Context) {
	group := c.Query("group")
	if group == "" {
		group = "default"
	}
	modelName := c.Query("model")
	if modelName == "" {
		common.ApiError(c, errors.New("model query parameter is required"))
		return
	}
	used := make(map[int]bool)
	for _, id := range c.QueryArray("used") {
		if v, err := strconv.Atoi(id); err == nil {
			used[v] = true
		}
	}
	diagnoses := service.DiagnosePoolMembers(group, modelName, used)
	_, poolOk := pool_setting.GetPoolByTag("")
	tier, tierErr := model.GetPriorityTierCandidates(group, modelName, 0, nil)
	tierIds := make([]int, 0, len(tier))
	for _, ch := range tier {
		tierIds = append(tierIds, ch.Id)
	}
	common.ApiSuccess(c, gin.H{
		"group":         group,
		"model":         modelName,
		"pool_matching": poolOk,
		"tier":          tierIds,
		"tier_error":    fmt.Sprintf("%v", tierErr),
		"price_known": func() bool {
			_, ok, _ := ratio_setting.GetModelRatio(modelName)
			if !ok {
				_, ok = ratio_setting.GetModelPrice(modelName, false)
			}
			return ok
		}(),
		"members": diagnoses,
	})
}

// TriggerPoolQuotaCollection runs one collection pass immediately.
func TriggerPoolQuotaCollection(c *gin.Context) {
	summary := service.RunPoolQuotaCollectionForced()
	common.ApiSuccess(c, summary)
}

type consoleCredentialRequest struct {
	Provider     string `json:"provider"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	RefreshToken string `json:"refresh_token"`
}

// SaveChannelConsoleCredential stores the second (console) credential of a
// channel. The relay key stays untouched; values never round-trip to the
// browser afterwards.
func SaveChannelConsoleCredential(c *gin.Context) {
	channelId, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelId <= 0 {
		common.ApiError(c, errors.New("invalid channel id"))
		return
	}
	var req consoleCredentialRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiError(c, err)
		return
	}
	if req.Provider == "" || (req.Username == "" && req.RefreshToken == "") {
		common.ApiError(c, errors.New("provider and username or refresh_token are required"))
		return
	}
	cred := &model.ChannelConsoleCredential{
		ChannelId:    channelId,
		Provider:     req.Provider,
		Username:     req.Username,
		Password:     req.Password,
		RefreshToken: req.RefreshToken,
	}
	if err := model.SaveChannelConsoleCredential(cred); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}

// GetChannelConsoleCredential returns credential presence with secrets masked.
func GetChannelConsoleCredential(c *gin.Context) {
	channelId, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelId <= 0 {
		common.ApiError(c, errors.New("invalid channel id"))
		return
	}
	cred, err := model.GetChannelConsoleCredential(channelId)
	if err != nil {
		common.ApiSuccess(c, nil)
		return
	}
	common.ApiSuccess(c, gin.H{
		"provider":          cred.Provider,
		"username":          cred.Username,
		"has_password":      cred.Password != "",
		"has_refresh_token": cred.RefreshToken != "",
		"token_expires_at":  cred.TokenExpiresAt,
		"updated_time":      cred.UpdatedTime,
	})
}

// DeleteChannelConsoleCredential removes the console credential of a channel.
func DeleteChannelConsoleCredential(c *gin.Context) {
	channelId, err := strconv.Atoi(c.Param("id"))
	if err != nil || channelId <= 0 {
		common.ApiError(c, errors.New("invalid channel id"))
		return
	}
	if err := model.DeleteChannelConsoleCredential(channelId); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, nil)
}
