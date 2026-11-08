package model

import (
	"os"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestPoolMemberSnapshotDatabaseMatrix verifies the pool member snapshot table
// on real databases: AutoMigrate is idempotent and the (channel_id, key_index)
// upsert keeps exactly one row per member. SQLite runs always; MySQL and
// PostgreSQL run when TEST_MYSQL_DSN / TEST_POSTGRES_DSN are configured.
func TestPoolMemberSnapshotDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN not configured")
				}
				driver = postgres.Open(dsn)
			default:
				t.Fatalf("unknown dialect %q", dialect)
			}

			db, err := gorm.Open(driver, &gorm.Config{})
			require.NoError(t, err)
			if dialect != "sqlite" {
				sqlDB, err := db.DB()
				require.NoError(t, err)
				t.Cleanup(func() { _ = sqlDB.Close() })
			}

			// Migration must succeed twice: AutoMigrate runs on every startup.
			require.NoError(t, db.AutoMigrate(&PoolMemberSnapshot{}))
			require.NoError(t, db.AutoMigrate(&PoolMemberSnapshot{}))

			snapshot := &PoolMemberSnapshot{
				ChannelId: 42,
				KeyIndex:  3,
				Tag:       "pooltest",
				Windows: PoolMemberWindows{{
					Remaining:   123.5,
					Capacity:    1000,
					ExpiryAt:    time.Now().Add(time.Hour).Truncate(time.Second),
					ShortWindow: true,
					Source:      "collector",
				}},
			}
			require.NoError(t, UpsertPoolMemberSnapshotWithDB(db, snapshot))

			// Upserting the same member updates instead of duplicating.
			snapshot.Windows = PoolMemberWindows{{
				Remaining: 77,
				Capacity:  1000,
				Source:    "collector",
			}}
			snapshot.QuotaUnknown = true
			require.NoError(t, UpsertPoolMemberSnapshotWithDB(db, snapshot))

			var count int64
			require.NoError(t, db.Model(&PoolMemberSnapshot{}).
				Where("channel_id = ? AND key_index = ?", 42, 3).Count(&count).Error)
			assert.Equal(t, int64(1), count)

			var stored PoolMemberSnapshot
			require.NoError(t, db.Where("channel_id = ? AND key_index = ?", 42, 3).First(&stored).Error)
			assert.Equal(t, "pooltest", stored.Tag)
			assert.True(t, stored.QuotaUnknown)
			require.Len(t, stored.Windows, 1)
			assert.Equal(t, 77.0, stored.Windows[0].Remaining)
			assert.Equal(t, 1000.0, stored.Windows[0].Capacity)
			assert.Equal(t, "collector", stored.Windows[0].Source)

			// Negative key indexes (single-key members) persist and match.
			single := &PoolMemberSnapshot{
				ChannelId: 43,
				KeyIndex:  -1,
				Tag:       "pooltest",
				Windows:   PoolMemberWindows{{Remaining: 1, Source: "balance"}},
			}
			require.NoError(t, UpsertPoolMemberSnapshotWithDB(db, single))
			require.NoError(t, UpsertPoolMemberSnapshotWithDB(db, single))
			require.NoError(t, db.Model(&PoolMemberSnapshot{}).
				Where("channel_id = ? AND key_index = ?", 43, -1).Count(&count).Error)
			assert.Equal(t, int64(1), count)

			require.NoError(t, DeletePoolMemberSnapshotsByChannelWithDB(db, 42))
			require.NoError(t, db.Model(&PoolMemberSnapshot{}).
				Where("channel_id = ?", 42).Count(&count).Error)
			assert.Equal(t, int64(0), count)
		})
	}
}
