package model

import (
	"database/sql/driver"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

// PoolMemberWindow is one quota window of a pool member as collected from the
// upstream or configured manually by the administrator. Remaining uses the
// collector's units; only comparisons inside one pool are meaningful.
type PoolMemberWindow struct {
	Remaining   float64   `json:"remaining"`
	Capacity    float64   `json:"capacity,omitempty"`
	ExpiryAt    time.Time `json:"expiry_at,omitempty"`
	ShortWindow bool      `json:"short_window,omitempty"`
	Source      string    `json:"source,omitempty"` // manual | collector | balance
}

// PoolMemberWindows is a JSON column holding one member's quota windows.
type PoolMemberWindows []PoolMemberWindow

// Value implements driver.Valuer. It must return a string, not []byte: under
// the PostgreSQL simple protocol a []byte argument is encoded as bytea, which
// writing into a json column triggers SQLSTATE 22P02.
func (w PoolMemberWindows) Value() (driver.Value, error) {
	b, err := common.Marshal(&w)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner.
func (w *PoolMemberWindows) Scan(value any) error {
	return common.Unmarshal(jsonScanBytes(value), w)
}

// PoolMemberSnapshot persists the latest quota windows of one pool member so
// window-aware strategies survive restarts and share state across instances.
// KeyIndex -1 denotes a single-key channel.
type PoolMemberSnapshot struct {
	Id           int               `json:"id" gorm:"primaryKey"`
	ChannelId    int               `json:"channel_id" gorm:"uniqueIndex:pool_member_unique,idx:pool_member_channel;not null"`
	KeyIndex     int               `json:"key_index" gorm:"uniqueIndex:pool_member_unique;not null;default:-1"`
	Tag          string            `json:"tag" gorm:"index"`
	Windows      PoolMemberWindows `json:"windows" gorm:"type:json"`
	QuotaUnknown bool              `json:"quota_unknown"`
	UpdatedTime  int64             `json:"updated_time" gorm:"bigint"`
}

// UpsertPoolMemberSnapshot writes the latest snapshot of one member. The
// (channel_id, key_index) pair is the member identity.
func UpsertPoolMemberSnapshot(snapshot *PoolMemberSnapshot) error {
	return UpsertPoolMemberSnapshotWithDB(DB, snapshot)
}

// UpsertPoolMemberSnapshotWithDB is the transaction seam shared with the
// dialect matrix test.
func UpsertPoolMemberSnapshotWithDB(db *gorm.DB, snapshot *PoolMemberSnapshot) error {
	snapshot.UpdatedTime = time.Now().Unix()
	var existing PoolMemberSnapshot
	err := db.Where("channel_id = ? AND key_index = ?", snapshot.ChannelId, snapshot.KeyIndex).First(&existing).Error
	if err != nil {
		return db.Create(snapshot).Error
	}
	snapshot.Id = existing.Id
	return db.Model(&existing).Select("tag", "windows", "quota_unknown", "updated_time").Updates(snapshot).Error
}

// GetPoolMemberSnapshotsByTag loads every member snapshot of a pool.
func GetPoolMemberSnapshotsByTag(tag string) ([]*PoolMemberSnapshot, error) {
	var snapshots []*PoolMemberSnapshot
	err := DB.Where("tag = ?", tag).Find(&snapshots).Error
	return snapshots, err
}

// GetAllPoolMemberSnapshots loads every stored snapshot for cache warm-up.
func GetAllPoolMemberSnapshots() ([]*PoolMemberSnapshot, error) {
	var snapshots []*PoolMemberSnapshot
	err := DB.Find(&snapshots).Error
	return snapshots, err
}

// DeletePoolMemberSnapshotsByChannel removes the snapshots of a channel whose
// keys were reorganized or whose tag left a pool.
func DeletePoolMemberSnapshotsByChannel(channelId int) error {
	return DeletePoolMemberSnapshotsByChannelWithDB(DB, channelId)
}

// DeletePoolMemberSnapshotsByChannelWithDB is the transaction seam shared
// with the dialect matrix test.
func DeletePoolMemberSnapshotsByChannelWithDB(db *gorm.DB, channelId int) error {
	return db.Where("channel_id = ?", channelId).Delete(&PoolMemberSnapshot{}).Error
}
