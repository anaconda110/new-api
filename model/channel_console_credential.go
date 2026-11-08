package model

import (
	"time"
)

// ChannelConsoleCredential is the second credential set of a channel: the
// upstream console login used to read quota windows, kept strictly apart from
// the relay key (some platforms expose no API-key quota endpoint at all).
// Values stay server-side; the management API only ever returns presence and
// a masked username.
type ChannelConsoleCredential struct {
	Id             int    `json:"id" gorm:"primaryKey"`
	ChannelId      int    `json:"channel_id" gorm:"uniqueIndex;not null"`
	Provider       string `json:"provider" gorm:"type:varchar(32);not null"`
	Username       string `json:"username" gorm:"type:varchar(128)"`
	Password       string `json:"password" gorm:"type:varchar(128)"`
	RefreshToken   string `json:"refresh_token" gorm:"type:varchar(1024)"`
	AccessToken    string `json:"-" gorm:"type:varchar(1024)"`
	TokenExpiresAt int64  `json:"token_expires_at" gorm:"bigint"`
	UpdatedTime    int64  `json:"updated_time" gorm:"bigint"`
}

// SaveChannelConsoleCredential upserts the credential of one channel.
func SaveChannelConsoleCredential(cred *ChannelConsoleCredential) error {
	cred.UpdatedTime = time.Now().Unix()
	var existing ChannelConsoleCredential
	err := DB.Where("channel_id = ?", cred.ChannelId).First(&existing).Error
	if err != nil {
		return DB.Create(cred).Error
	}
	cred.Id = existing.Id
	return DB.Model(&existing).Select("provider", "username", "password", "refresh_token", "access_token", "token_expires_at", "updated_time").Updates(cred).Error
}

// GetChannelConsoleCredential loads the credential of one channel; nil when
// the channel has none.
func GetChannelConsoleCredential(channelId int) (*ChannelConsoleCredential, error) {
	var cred ChannelConsoleCredential
	err := DB.Where("channel_id = ?", channelId).First(&cred).Error
	if err != nil {
		return nil, err
	}
	return &cred, nil
}

// UpdateChannelConsoleCredentialTokens persists refreshed tokens so the next
// collection round can reuse them instead of logging in again.
func UpdateChannelConsoleCredentialTokens(channelId int, accessToken string, refreshToken string, expiresAt int64) error {
	return DB.Model(&ChannelConsoleCredential{}).Where("channel_id = ?", channelId).
		Select("access_token", "refresh_token", "token_expires_at", "updated_time").
		Updates(ChannelConsoleCredential{AccessToken: accessToken, RefreshToken: refreshToken, TokenExpiresAt: expiresAt, UpdatedTime: time.Now().Unix()}).Error
}

// DeleteChannelConsoleCredential removes the credential of one channel.
func DeleteChannelConsoleCredential(channelId int) error {
	return DB.Where("channel_id = ?", channelId).Delete(&ChannelConsoleCredential{}).Error
}
